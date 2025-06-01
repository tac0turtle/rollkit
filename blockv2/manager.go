package blockv2

import (
	"context"
	"fmt"
	"time"

	"cosmossdk.io/log"
	goheader "github.com/celestiaorg/go-header"
	ds "github.com/ipfs/go-datastore"
	libcrypto "github.com/libp2p/go-libp2p/core/crypto"

	"github.com/rollkit/rollkit/blockv2/actor"
	"github.com/rollkit/rollkit/blockv2/actors"
	coreda "github.com/rollkit/rollkit/core/da"
	coreexecutor "github.com/rollkit/rollkit/core/execution"
	coresequencer "github.com/rollkit/rollkit/core/sequencer"
	"github.com/rollkit/rollkit/pkg/cache"
	"github.com/rollkit/rollkit/pkg/config"
	"github.com/rollkit/rollkit/pkg/genesis"
	"github.com/rollkit/rollkit/pkg/signer"
	"github.com/rollkit/rollkit/pkg/store"
	"github.com/rollkit/rollkit/types"
)

// Manager is the v2 block manager using actor model
type Manager struct {
	// Actor system
	system *actor.System

	// Actor references
	stateActor      *actor.PID
	syncActor       *actor.PID
	aggregatorActor *actor.PID
	producerActor   *actor.PID
	validatorActor  *actor.PID
	retrieverActor  *actor.PID
	submitterActor  *actor.PID
	reaperActor     *actor.PID
	daIncluderActor *actor.PID
	storeSyncActor  *actor.PID

	// Configuration
	config  config.Config
	genesis genesis.Genesis

	// Dependencies
	store     store.Store
	exec      coreexecutor.Executor
	sequencer coresequencer.Sequencer
	da        coreda.DA
	signer    signer.Signer
	logger    log.Logger

	// P2P stores
	headerStore goheader.Store[*types.SignedHeader]
	dataStore   goheader.Store[*types.Data]
}

// NewManager creates a new v2 manager
func NewManager(
	ctx context.Context,
	signer signer.Signer,
	config config.Config,
	genesis genesis.Genesis,
	store store.Store,
	exec coreexecutor.Executor,
	sequencer coresequencer.Sequencer,
	da coreda.DA,
	logger log.Logger,
	headerStore goheader.Store[*types.SignedHeader],
	dataStore goheader.Store[*types.Data],
	headerBroadcaster broadcaster[*types.SignedHeader],
	dataBroadcaster broadcaster[*types.Data],
	seqMetrics *Metrics,
	gasPrice float64,
	gasMultiplier float64,
) (*Manager, error) {
	// Get initial state
	initialState, err := getInitialState(ctx, genesis, signer, store, exec, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to get initial state: %w", err)
	}

	// Set initial height in store
	if err := store.SetHeight(ctx, initialState.LastBlockHeight); err != nil {
		return nil, err
	}

	// Create actor system
	system := actor.NewSystem(ctx, logger,
		actor.WithMailboxSize(1000))

	// Create manager
	m := &Manager{
		system:      system,
		config:      config,
		genesis:     genesis,
		store:       store,
		exec:        exec,
		sequencer:   sequencer,
		da:          da,
		signer:      signer,
		logger:      logger,
		headerStore: headerStore,
		dataStore:   dataStore,
	}

	// Spawn actors
	if err := m.spawnActors(initialState); err != nil {
		return nil, err
	}

	return m, nil
}

// spawnActors creates and starts all actors
func (m *Manager) spawnActors(initialState types.State) error {
	var err error

	// 1. State Actor - source of truth for blockchain state
	m.stateActor, err = m.system.Spawn("state",
		actors.NewStateActor(m.store, initialState),
		actor.WithSupervisor(&actor.AlwaysRestartSupervisor{Delay: time.Second}))
	if err != nil {
		return fmt.Errorf("failed to spawn state actor: %w", err)
	}

	// 2. Validator Actor - validates blocks
	m.validatorActor, err = m.system.Spawn("validator",
		actors.NewValidatorActor(
			m.genesis.ChainID,
			m.genesis.ProposerAddress,
			m.stateActor,
		))
	if err != nil {
		return fmt.Errorf("failed to spawn validator actor: %w", err)
	}

	// 3. Sync Actor - handles block synchronization
	m.syncActor, err = m.system.Spawn("sync",
		actors.NewSyncActor(
			m.store,
			m.exec,
			m.genesis,
			m.stateActor,
			m.validatorActor,
		))
	if err != nil {
		return fmt.Errorf("failed to spawn sync actor: %w", err)
	}

	// 4. Producer Actor - creates blocks
	m.producerActor, err = m.system.Spawn("producer",
		actors.NewProducerActor(
			m.store,
			m.signer,
			m.genesis,
			m.sequencer,
			m.config.Node.MaxPendingHeaders,
			m.stateActor,
			nil, // sequencerActor - will be set later
			m.syncActor,
			nil, // submitterActor - will be set later
		))
	if err != nil {
		return fmt.Errorf("failed to spawn producer actor: %w", err)
	}

	// 5. Aggregator Actor - controls block production timing
	m.aggregatorActor, err = m.system.Spawn("aggregator",
		actors.NewAggregatorActor(
			m.config,
			m.genesis,
			m.producerActor,
			m.stateActor,
		))
	if err != nil {
		return fmt.Errorf("failed to spawn aggregator actor: %w", err)
	}

	// 6. Retriever Actor - fetches from DA
	m.retrieverActor, err = m.system.Spawn("retriever",
		actors.NewRetrieverActor(
			m.da,
			m.genesis.ChainID,
			m.genesis.ProposerAddress,
			initialState.DAHeight,
			m.syncActor,
		))
	if err != nil {
		return fmt.Errorf("failed to spawn retriever actor: %w", err)
	}

	// 7. DA Submitter Actor - submits to DA
	m.submitterActor, err = m.system.Spawn("submitter",
		actors.NewDASubmitterActor(
			m.da,
			m.store,
			m.config.DA.GasPrice,
			m.config.DA.GasMultiplier,
			m.config.DA.BlockTime.Duration,
			m.config.DA.MempoolTTL,
			nil, // daIncluderActor - will be set later
		))
	if err != nil {
		return fmt.Errorf("failed to spawn submitter actor: %w", err)
	}

	// 8. DA Includer Actor - tracks DA inclusion
	// Note: In production, caches would be passed via messages or shared through a cache actor
	headerCache := cache.NewCache[types.SignedHeader]()
	dataCache := cache.NewCache[types.Data]()
	m.daIncluderActor, err = m.system.Spawn("da-includer",
		actors.NewDAIncluderActor(
			m.store,
			m.exec,
			headerCache,
			dataCache,
			m.syncActor,
		))
	if err != nil {
		return fmt.Errorf("failed to spawn da includer actor: %w", err)
	}

	// Update submitter with da includer reference
	// This would need a message in production

	// 9. Reaper Actor - collects transactions
	// Note: In production, get datastore from store interface
	var seenStore ds.Batching
	if defaultStore, ok := m.store.(interface{ GetDatastore() ds.Batching }); ok {
		seenStore = defaultStore.GetDatastore()
	} else {
		// Create a simple in-memory datastore for testing
		seenStore = ds.NewMapDatastore()
	}
	m.reaperActor, err = m.system.Spawn("reaper",
		actors.NewReaperActor(
			m.exec,
			m.sequencer,
			m.genesis.ChainID,
			time.Second,
			seenStore,
			m.aggregatorActor,
			m.submitterActor,
		))
	if err != nil {
		return fmt.Errorf("failed to spawn reaper actor: %w", err)
	}

	// 10. Store Sync Actor - syncs from P2P stores
	m.storeSyncActor, err = m.system.Spawn("store-sync",
		actors.NewStoreSyncActor(
			m.headerStore,
			m.dataStore,
			m.store,
			m.genesis.ProposerAddress,
			m.syncActor,
			m.validatorActor,
		))
	if err != nil {
		return fmt.Errorf("failed to spawn store sync actor: %w", err)
	}

	m.logger.Info("all actors spawned successfully")

	return nil
}

// Start begins all manager operations
func (m *Manager) Start(ctx context.Context) error {
	// Actors are already started via the system
	m.logger.Info("block manager v2 started")
	return nil
}

// Stop gracefully shuts down the manager
func (m *Manager) Stop() error {
	m.logger.Info("stopping block manager v2")
	return m.system.Shutdown(30 * time.Second)
}

// GetLastState returns the current blockchain state
func (m *Manager) GetLastState() (types.State, error) {
	reply := make(chan types.State, 1)
	err := m.stateActor.Tell(actors.GetState{Reply: reply})
	if err != nil {
		return types.State{}, err
	}

	select {
	case state := <-reply:
		return state, nil
	case <-time.After(5 * time.Second):
		return types.State{}, fmt.Errorf("timeout getting state")
	}
}

// NotifyNewTransactions signals that new transactions are available
func (m *Manager) NotifyNewTransactions() {
	if err := m.aggregatorActor.Tell(actors.TxAvailable{}); err != nil {
		m.logger.Error("failed to notify tx available", "error", err)
	}
}

// ProduceBlock manually triggers block production
func (m *Manager) ProduceBlock(ctx context.Context) error {
	reply := make(chan *actors.ProduceBlockResult, 1)
	err := m.aggregatorActor.Tell(actors.ProduceBlock{
		Timestamp: time.Now(),
		Reply:     reply,
	})
	if err != nil {
		return err
	}

	select {
	case result := <-reply:
		return result.Error
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IsBlockHashSeen checks if a block hash has been seen
func (m *Manager) IsBlockHashSeen(blockHash string) (bool, error) {
	reply := make(chan any, 1)
	err := m.syncActor.Tell(actors.GetCacheEntry{
		Key:   "seen:" + blockHash,
		Reply: reply,
	})
	if err != nil {
		return false, err
	}

	select {
	case result := <-reply:
		if result == nil {
			return false, nil
		}
		seen, ok := result.(bool)
		return ok && seen, nil
	case <-time.After(time.Second):
		return false, fmt.Errorf("timeout checking block hash")
	}
}

// GetStoreHeight returns the current store height
func (m *Manager) GetStoreHeight(ctx context.Context) (uint64, error) {
	return m.store.Height(ctx)
}

// GetExecutor returns the execution client
func (m *Manager) GetExecutor() coreexecutor.Executor {
	return m.exec
}

// Helper functions from v1

type broadcaster[T any] interface {
	WriteToStoreAndBroadcast(ctx context.Context, payload T) error
}

// getInitialState loads or creates the initial blockchain state
func getInitialState(
	ctx context.Context,
	genesis genesis.Genesis,
	signer signer.Signer,
	store store.Store,
	exec coreexecutor.Executor,
	logger log.Logger,
) (types.State, error) {
	// Load state from store
	s, err := store.GetState(ctx)
	if err == nil {
		// Validate against genesis
		if uint64(genesis.InitialHeight) > s.LastBlockHeight {
			return types.State{}, fmt.Errorf(
				"genesis.InitialHeight (%d) > stored height (%d)",
				genesis.InitialHeight, s.LastBlockHeight)
		}
		return s, nil
	}

	// Create new state from genesis
	logger.Info("No state found, initializing from genesis")

	stateRoot, _, err := exec.InitChain(ctx,
		genesis.GenesisDAStartTime,
		genesis.InitialHeight,
		genesis.ChainID)
	if err != nil {
		return types.State{}, fmt.Errorf("failed to init chain: %w", err)
	}

	// Create genesis header
	header := types.Header{
		AppHash:         stateRoot,
		DataHash:        new(types.Data).DACommitment(),
		ProposerAddress: genesis.ProposerAddress,
		BaseHeader: types.BaseHeader{
			ChainID: genesis.ChainID,
			Height:  genesis.InitialHeight,
			Time:    uint64(genesis.GenesisDAStartTime.UnixNano()),
		},
	}

	var signature types.Signature
	var pubKey libcrypto.PubKey

	if signer != nil {
		pubKey, err = signer.GetPublic()
		if err != nil {
			return types.State{}, fmt.Errorf("failed to get public key: %w", err)
		}

		b, err := header.MarshalBinary()
		if err != nil {
			return types.State{}, err
		}

		signature, err = signer.Sign(b)
		if err != nil {
			return types.State{}, fmt.Errorf("failed to sign header: %w", err)
		}
	}

	// Save genesis block
	signedHeader := &types.SignedHeader{
		Header: header,
		Signer: types.Signer{
			PubKey:  pubKey,
			Address: genesis.ProposerAddress,
		},
		Signature: signature,
	}

	err = store.SaveBlockData(ctx, signedHeader, &types.Data{}, &signature)
	if err != nil {
		return types.State{}, fmt.Errorf("failed to save genesis: %w", err)
	}

	return types.State{
		Version:         types.Version{},
		ChainID:         genesis.ChainID,
		InitialHeight:   genesis.InitialHeight,
		LastBlockHeight: genesis.InitialHeight - 1,
		LastBlockTime:   genesis.GenesisDAStartTime,
		AppHash:         stateRoot,
		DAHeight:        0,
	}, nil
}

// Metrics placeholder
type Metrics struct{}

// Additional actor runners would be added here for:
// - AggregationLoop -> handled by aggregatorActor
// - SyncLoop -> handled by syncActor
// - RetrieveLoop -> handled by retrieverActor
// - HeaderSubmissionLoop -> handled by submitterActor
// - BatchSubmissionLoop -> handled by submitterActor
// - DAIncluderLoop -> handled by daIncluderActor
// - HeaderStoreRetrieveLoop -> handled by storeSyncActor
// - DataStoreRetrieveLoop -> handled by storeSyncActor

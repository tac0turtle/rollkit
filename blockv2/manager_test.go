package blockv2_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	goheader "github.com/celestiaorg/go-header"
	ds "github.com/ipfs/go-datastore"

	"github.com/rollkit/rollkit/blockv2"
	coreda "github.com/rollkit/rollkit/core/da"
	coresequencer "github.com/rollkit/rollkit/core/sequencer"
	"github.com/rollkit/rollkit/pkg/config"
	"github.com/rollkit/rollkit/pkg/genesis"
	"github.com/rollkit/rollkit/pkg/store"
	"github.com/rollkit/rollkit/types"
)

func TestManager(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create dependencies
	mockStore := &mockManagerStore{
		height:   0,
		blocks:   make(map[uint64]*types.SignedHeader),
		data:     make(map[uint64]*types.Data),
		metadata: make(map[string][]byte),
	}
	mockSigner := &mockManagerSigner{}
	mockExecutor := &mockManagerExecutor{}
	mockSequencer := &mockManagerSequencer{}
	mockDA := &mockManagerDA{}

	cfg := config.Config{
		Node: config.NodeConfig{
			MaxPendingHeaders: 100,
		},
		Rollkit: config.RollkitConfig{
			DAConfig: config.DAConfig{
				BlockTime:     config.Duration{Duration: 100 * time.Millisecond},
				GasPrice:      1.0,
				GasMultiplier: 1.5,
				MempoolTTL:    3,
			},
		},
	}

	gen := genesis.Genesis{
		ChainID:            "test-chain",
		InitialHeight:      1,
		ProposerAddress:    []byte("proposer"),
		GenesisDAStartTime: time.Now(),
	}

	logger := log.NewNopLogger()
	headerStore := &mockHeaderStore{}
	dataStore := &mockDataStore{}
	metrics := &blockv2.Metrics{}

	t.Run("CreateManager", func(t *testing.T) {
		manager, err := blockv2.NewManager(
			ctx,
			mockSigner,
			cfg,
			gen,
			mockStore,
			mockExecutor,
			mockSequencer,
			mockDA,
			logger,
			headerStore,
			dataStore,
			nil, // headerBroadcaster
			nil, // dataBroadcaster
			metrics,
			1.0, // gasPrice
			1.5, // gasMultiplier
		)
		require.NoError(t, err)
		require.NotNil(t, manager)

		// Test manager start
		err = manager.Start(ctx)
		assert.NoError(t, err)

		// Test manager methods
		t.Run("GetLastState", func(t *testing.T) {
			state, err := manager.GetLastState()
			assert.NoError(t, err)
			assert.Equal(t, "test-chain", state.ChainID)
		})

		t.Run("ProduceBlock", func(t *testing.T) {
			err := manager.ProduceBlock(ctx)
			assert.NoError(t, err)
		})

		t.Run("NotifyNewTransactions", func(t *testing.T) {
			// Should not panic
			manager.NotifyNewTransactions()
		})

		t.Run("GetStoreHeight", func(t *testing.T) {
			height, err := manager.GetStoreHeight(ctx)
			assert.NoError(t, err)
			assert.GreaterOrEqual(t, height, uint64(0))
		})

		t.Run("GetExecutor", func(t *testing.T) {
			executor := manager.GetExecutor()
			assert.NotNil(t, executor)
			assert.Equal(t, mockExecutor, executor)
		})

		t.Run("IsBlockHashSeen", func(t *testing.T) {
			seen, err := manager.IsBlockHashSeen("test-hash")
			assert.NoError(t, err)
			assert.False(t, seen) // Should be false for new hash
		})

		// Test manager stop
		err = manager.Stop()
		assert.NoError(t, err)
	})

	t.Run("ManagerWithExistingState", func(t *testing.T) {
		// Set up store with existing state
		existingState := types.State{
			ChainID:         "test-chain",
			LastBlockHeight: 10,
			InitialHeight:   1,
			AppHash:         []byte("existing-hash"),
		}
		mockStore.state = existingState

		manager, err := blockv2.NewManager(
			ctx,
			mockSigner,
			cfg,
			gen,
			mockStore,
			mockExecutor,
			mockSequencer,
			mockDA,
			logger,
			headerStore,
			dataStore,
			nil,
			nil,
			metrics,
			1.0,
			1.5,
		)
		require.NoError(t, err)

		state, err := manager.GetLastState()
		assert.NoError(t, err)
		assert.Equal(t, uint64(10), state.LastBlockHeight)
		assert.Equal(t, []byte("existing-hash"), state.AppHash)

		err = manager.Stop()
		assert.NoError(t, err)
	})

	t.Run("ManagerBlockProduction", func(t *testing.T) {
		// Reset store
		mockStore = &mockManagerStore{
			height:   0,
			blocks:   make(map[uint64]*types.SignedHeader),
			data:     make(map[uint64]*types.Data),
			metadata: make(map[string][]byte),
		}

		manager, err := blockv2.NewManager(
			ctx,
			mockSigner,
			cfg,
			gen,
			mockStore,
			mockExecutor,
			mockSequencer,
			mockDA,
			logger,
			headerStore,
			dataStore,
			nil,
			nil,
			metrics,
			1.0,
			1.5,
		)
		require.NoError(t, err)

		err = manager.Start(ctx)
		require.NoError(t, err)

		// Produce multiple blocks
		for i := 0; i < 3; i++ {
			err := manager.ProduceBlock(ctx)
			assert.NoError(t, err)
		}

		// Check that blocks were produced
		height, err := manager.GetStoreHeight(ctx)
		assert.NoError(t, err)
		assert.Greater(t, height, uint64(0))

		err = manager.Stop()
		assert.NoError(t, err)
	})
}

func TestManagerEdgeCases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("InvalidGenesis", func(t *testing.T) {
		mockStore := &mockManagerStore{}
		mockSigner := &mockManagerSigner{}
		mockExecutor := &mockManagerExecutor{shouldFail: true}

		cfg := config.Config{}
		gen := genesis.Genesis{
			ChainID:       "test-chain",
			InitialHeight: 1,
		}

		logger := log.NewNopLogger()

		_, err := blockv2.NewManager(
			ctx,
			mockSigner,
			cfg,
			gen,
			mockStore,
			mockExecutor,
			&mockManagerSequencer{},
			&mockManagerDA{},
			logger,
			&mockHeaderStore{},
			&mockDataStore{},
			nil,
			nil,
			&blockv2.Metrics{},
			1.0,
			1.5,
		)
		assert.Error(t, err)
	})

	t.Run("ConcurrentOperations", func(t *testing.T) {
		mockStore := &mockManagerStore{
			height:   0,
			blocks:   make(map[uint64]*types.SignedHeader),
			data:     make(map[uint64]*types.Data),
			metadata: make(map[string][]byte),
		}

		cfg := config.Config{
			Node: config.NodeConfig{MaxPendingHeaders: 100},
			Rollkit: config.RollkitConfig{
				DAConfig: config.DAConfig{
					BlockTime: config.Duration{Duration: 50 * time.Millisecond},
				},
			},
		}

		gen := genesis.Genesis{
			ChainID:            "test-chain",
			InitialHeight:      1,
			ProposerAddress:    []byte("proposer"),
			GenesisDAStartTime: time.Now(),
		}

		manager, err := blockv2.NewManager(
			ctx,
			&mockManagerSigner{},
			cfg,
			gen,
			mockStore,
			&mockManagerExecutor{},
			&mockManagerSequencer{},
			&mockManagerDA{},
			log.NewNopLogger(),
			&mockHeaderStore{},
			&mockDataStore{},
			nil,
			nil,
			&blockv2.Metrics{},
			1.0,
			1.5,
		)
		require.NoError(t, err)

		err = manager.Start(ctx)
		require.NoError(t, err)

		// Run concurrent operations
		const numOps = 10
		errChan := make(chan error, numOps*3)

		// Concurrent block production
		for i := 0; i < numOps; i++ {
			go func() {
				errChan <- manager.ProduceBlock(ctx)
			}()
		}

		// Concurrent state queries
		for i := 0; i < numOps; i++ {
			go func() {
				_, err := manager.GetLastState()
				errChan <- err
			}()
		}

		// Concurrent height queries
		for i := 0; i < numOps; i++ {
			go func() {
				_, err := manager.GetStoreHeight(ctx)
				errChan <- err
			}()
		}

		// Collect results
		for i := 0; i < numOps*3; i++ {
			select {
			case err := <-errChan:
				assert.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("timeout waiting for concurrent operations")
			}
		}

		err = manager.Stop()
		assert.NoError(t, err)
	})
}

// Mock implementations

type mockManagerStore struct {
	height   uint64
	blocks   map[uint64]*types.SignedHeader
	data     map[uint64]*types.Data
	metadata map[string][]byte
	state    types.State
}

func (m *mockManagerStore) Height(ctx context.Context) (uint64, error) {
	return m.height, nil
}

func (m *mockManagerStore) SetHeight(ctx context.Context, height uint64) error {
	m.height = height
	return nil
}

func (m *mockManagerStore) GetBlockData(ctx context.Context, height uint64) (*types.SignedHeader, *types.Data, error) {
	header, ok := m.blocks[height]
	if !ok {
		return nil, nil, store.ErrNotFound
	}
	data := m.data[height]
	return header, data, nil
}

func (m *mockManagerStore) SaveBlockData(ctx context.Context, header *types.SignedHeader, data *types.Data, signature *types.Signature) error {
	m.blocks[header.Height()] = header
	m.data[header.Height()] = data
	return nil
}

func (m *mockManagerStore) GetSignature(ctx context.Context, height uint64) (*types.Signature, error) {
	return &types.Signature{}, nil
}

func (m *mockManagerStore) GetMetadata(ctx context.Context, key string) ([]byte, error) {
	if m.metadata == nil {
		m.metadata = make(map[string][]byte)
	}
	data, ok := m.metadata[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return data, nil
}

func (m *mockManagerStore) SetMetadata(ctx context.Context, key string, value []byte) error {
	if m.metadata == nil {
		m.metadata = make(map[string][]byte)
	}
	m.metadata[key] = value
	return nil
}

func (m *mockManagerStore) GetState(ctx context.Context) (types.State, error) {
	if (m.state == types.State{}) {
		return types.State{}, store.ErrNotFound
	}
	return m.state, nil
}

func (m *mockManagerStore) SaveState(ctx context.Context, state types.State) error {
	m.state = state
	return nil
}

func (m *mockManagerStore) GetDatastore() ds.Batching {
	return ds.NewMapDatastore()
}

type mockManagerSigner struct{}

func (m *mockManagerSigner) GetPublic() (any, error) {
	return &mockPubKey{}, nil
}

func (m *mockManagerSigner) GetAddress() ([]byte, error) {
	return []byte("proposer"), nil
}

func (m *mockManagerSigner) Sign(data []byte) (types.Signature, error) {
	return []byte("signature"), nil
}

type mockPubKey struct{}

func (m *mockPubKey) Verify(msg []byte, sig []byte) (bool, error) {
	return true, nil
}

func (m *mockPubKey) Bytes() []byte {
	return []byte("mock-pubkey")
}

func (m *mockPubKey) Equals(other interface{}) bool {
	return true
}

func (m *mockPubKey) Type() string {
	return "mock"
}

type mockManagerExecutor struct {
	shouldFail bool
}

func (m *mockManagerExecutor) InitChain(ctx context.Context, genesisTime time.Time, initialHeight uint64, chainID string) ([]byte, uint64, error) {
	if m.shouldFail {
		return nil, 0, assert.AnError
	}
	return []byte("genesis-hash"), 0, nil
}

func (m *mockManagerExecutor) GetTxs(ctx context.Context) ([][]byte, error) {
	return [][]byte{}, nil
}

func (m *mockManagerExecutor) ExecuteTxs(ctx context.Context, txs [][]byte, blockHeight uint64, timestamp time.Time, prevStateRoot []byte) ([]byte, uint64, error) {
	if m.shouldFail {
		return nil, 0, assert.AnError
	}
	return []byte("new-state-root"), 1000, nil
}

func (m *mockManagerExecutor) SetFinal(ctx context.Context, blockHeight uint64) error {
	return nil
}

type mockManagerSequencer struct{}

func (m *mockManagerSequencer) GetNextBatch(ctx context.Context, req coresequencer.GetNextBatchRequest) (*coresequencer.GetNextBatchResponse, error) {
	return &coresequencer.GetNextBatchResponse{
		Batch: &coresequencer.Batch{
			Transactions: [][]byte{},
		},
		Timestamp: time.Now(),
		BatchData: [][]byte{},
	}, nil
}

type mockManagerDA struct{}

func (m *mockManagerDA) Submit(ctx context.Context, blobs []coreda.Blob, gasPrice float64, namespace coreda.Namespace) coreda.ResultSubmit {
	return coreda.ResultSubmit{
		Code:   coreda.StatusSuccess,
		Height: 1,
	}
}

func (m *mockManagerDA) Get(ctx context.Context, ids []coreda.ID, namespace coreda.Namespace) coreda.ResultGet {
	return coreda.ResultGet{Code: coreda.StatusSuccess}
}

func (m *mockManagerDA) GetProofs(ctx context.Context, ids []coreda.ID, namespace coreda.Namespace) coreda.ResultGetProofs {
	return coreda.ResultGetProofs{}
}

func (m *mockManagerDA) Commit(ctx context.Context, blocks []coreda.Block, namespace coreda.Namespace) coreda.ResultCommit {
	return coreda.ResultCommit{}
}

func (m *mockManagerDA) Validate(ctx context.Context, ids []coreda.ID, proofs []coreda.Proof, namespace coreda.Namespace) coreda.ResultValidate {
	return coreda.ResultValidate{}
}

type mockHeaderStore struct{}

func (m *mockHeaderStore) Init(ctx context.Context, initial *types.SignedHeader) error {
	return nil
}

func (m *mockHeaderStore) Height() uint64 {
	return 0
}

func (m *mockHeaderStore) Has(hash goheader.Hash) bool {
	return false
}

func (m *mockHeaderStore) HasAt(height uint64) bool {
	return false
}

func (m *mockHeaderStore) Get(hash goheader.Hash) (*types.SignedHeader, error) {
	return nil, nil
}

func (m *mockHeaderStore) GetByHeight(height uint64) (*types.SignedHeader, error) {
	return nil, nil
}

func (m *mockHeaderStore) GetRangeByHeight(from, to uint64) ([]*types.SignedHeader, error) {
	return nil, nil
}

func (m *mockHeaderStore) GetRange(from, to uint64) ([]*types.SignedHeader, error) {
	return nil, nil
}

func (m *mockHeaderStore) Head(ctx context.Context, headers ...*types.SignedHeader) (*types.SignedHeader, error) {
	return nil, nil
}

func (m *mockHeaderStore) Append(ctx context.Context, headers ...*types.SignedHeader) error {
	return nil
}

func (m *mockHeaderStore) Flush(ctx context.Context) error {
	return nil
}

type mockDataStore struct{}

func (m *mockDataStore) Init(ctx context.Context, initial *types.Data) error {
	return nil
}

func (m *mockDataStore) Height() uint64 {
	return 0
}

func (m *mockDataStore) Has(hash goheader.Hash) bool {
	return false
}

func (m *mockDataStore) HasAt(height uint64) bool {
	return false
}

func (m *mockDataStore) Get(hash goheader.Hash) (*types.Data, error) {
	return nil, nil
}

func (m *mockDataStore) GetByHeight(height uint64) (*types.Data, error) {
	return nil, nil
}

func (m *mockDataStore) GetRangeByHeight(from, to uint64) ([]*types.Data, error) {
	return nil, nil
}

func (m *mockDataStore) GetRange(from, to uint64) ([]*types.Data, error) {
	return nil, nil
}

func (m *mockDataStore) Head(ctx context.Context, headers ...*types.Data) (*types.Data, error) {
	return nil, nil
}

func (m *mockDataStore) Append(ctx context.Context, headers ...*types.Data) error {
	return nil
}

func (m *mockDataStore) Flush(ctx context.Context) error {
	return nil
}

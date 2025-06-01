package actors

import (
	"bytes"
	"context"
	"fmt"

	"github.com/rollkit/rollkit/blockv2/actor"
	coreexecutor "github.com/rollkit/rollkit/core/execution"
	"github.com/rollkit/rollkit/pkg/cache"
	"github.com/rollkit/rollkit/pkg/genesis"
	"github.com/rollkit/rollkit/pkg/store"
	"github.com/rollkit/rollkit/types"
)

var dataHashForEmptyTxs = []byte{110, 52, 11, 156, 255, 179, 122, 152, 156, 165, 68, 230, 187, 120, 10, 44, 120, 144, 29, 63, 179, 55, 56, 118, 133, 17, 163, 6, 23, 175, 160, 29}

// SyncActor manages block synchronization and application
type SyncActor struct {
	// Actor references
	stateActor     *actor.PID
	validatorActor *actor.PID

	// Dependencies
	store   store.Store
	exec    coreexecutor.Executor
	genesis genesis.Genesis

	// Caches
	headerCache *cache.Cache[types.SignedHeader]
	dataCache   *cache.Cache[types.Data]

	// Pending blocks waiting to be synced
	pendingHeaders map[uint64]*types.SignedHeader
	pendingData    map[uint64]*types.Data

	// Mapping data commitment to height for out-of-order arrivals
	dataCommitmentToHeight map[string]uint64

	// Circuit breaker for execution failures
	execCircuitBreaker *actor.CircuitBreaker
}

// NewSyncActor creates a new sync actor
func NewSyncActor(
	store store.Store,
	exec coreexecutor.Executor,
	genesis genesis.Genesis,
	stateActor *actor.PID,
	validatorActor *actor.PID,
) *SyncActor {
	return &SyncActor{
		stateActor:             stateActor,
		validatorActor:         validatorActor,
		store:                  store,
		exec:                   exec,
		genesis:                genesis,
		headerCache:            cache.NewCache[types.SignedHeader](),
		dataCache:              cache.NewCache[types.Data](),
		pendingHeaders:         make(map[uint64]*types.SignedHeader),
		pendingData:            make(map[uint64]*types.Data),
		dataCommitmentToHeight: make(map[string]uint64),
		execCircuitBreaker:     actor.NewCircuitBreaker("exec", 5, 30),
	}
}

// Receive processes incoming messages
func (s *SyncActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case actor.Started:
		ctx.Logger().Info("sync actor started")

	case HeaderFetched:
		s.handleHeaderFetched(ctx, m)

	case BatchFetched:
		s.handleBatchFetched(ctx, m)

	case ApplyBlock:
		result := s.applyBlock(ctx, m.Header, m.Data)
		select {
		case m.Reply <- result:
		default:
			ctx.Logger().Error("failed to send apply block reply")
		}

	case GetCacheEntry:
		value := s.getCacheEntry(m.Key)
		select {
		case m.Reply <- value:
		default:
			ctx.Logger().Error("failed to send cache reply")
		}

	case SetCacheEntry:
		s.setCacheEntry(m.Key, m.Value)

	case actor.Stopping:
		// Clean up resources
		s.pendingHeaders = nil
		s.pendingData = nil
		s.dataCommitmentToHeight = nil
	}
}

// handleHeaderFetched processes a new header
func (s *SyncActor) handleHeaderFetched(ctx actor.Context, msg HeaderFetched) {
	header := msg.Header
	height := header.Height()
	headerHash := header.Hash().String()

	ctx.Logger().Debug("header fetched",
		"height", height,
		"hash", headerHash,
		"daHeight", msg.DAHeight)

	// Check if already seen
	if s.headerCache.IsSeen(headerHash) {
		ctx.Logger().Debug("header already seen", "height", height)
		return
	}

	// Get current height
	currentHeight, err := s.store.Height(context.Background())
	if err != nil {
		ctx.Logger().Error("failed to get store height", "error", err)
		return
	}

	if height <= currentHeight {
		ctx.Logger().Debug("header already synced", "height", height)
		return
	}

	// Store in pending
	s.pendingHeaders[height] = header
	s.headerCache.SetItem(height, header)

	// Handle empty data hash
	if bytes.Equal(header.DataHash, dataHashForEmptyTxs) {
		s.pendingData[height] = &types.Data{
			Metadata: &types.Metadata{
				ChainID: header.ChainID(),
				Height:  height,
				Time:    header.BaseHeader.Time,
			},
		}
	} else {
		// Map data commitment to height for later matching
		dataHashStr := header.DataHash.String()
		if data := s.dataCache.GetItemByHash(dataHashStr); data != nil {
			s.pendingData[height] = data
		} else {
			s.dataCommitmentToHeight[dataHashStr] = height
		}
	}

	// Try to sync any pending blocks
	s.trySyncPendingBlocks(ctx)
}

// handleBatchFetched processes new batch data
func (s *SyncActor) handleBatchFetched(ctx actor.Context, msg BatchFetched) {
	data := msg.Data
	if len(data.Txs) == 0 {
		return
	}

	dataHash := data.DACommitment().String()
	ctx.Logger().Debug("batch fetched",
		"hash", dataHash,
		"daHeight", msg.DAHeight)

	// Check if already seen
	if s.dataCache.IsSeen(dataHash) {
		ctx.Logger().Debug("data already seen")
		return
	}

	// Check if we have a height mapping for this data
	if height, ok := s.dataCommitmentToHeight[dataHash]; ok {
		delete(s.dataCommitmentToHeight, dataHash)
		s.pendingData[height] = data
		s.dataCache.SetItem(height, data)

		// Try to sync pending blocks
		s.trySyncPendingBlocks(ctx)
	} else {
		// Cache by hash for later
		s.dataCache.SetItemByHash(dataHash, data)
	}
}

// trySyncPendingBlocks attempts to sync any ready blocks
func (s *SyncActor) trySyncPendingBlocks(ctx actor.Context) {
	currentHeight, err := s.store.Height(context.Background())
	if err != nil {
		ctx.Logger().Error("failed to get store height", "error", err)
		return
	}

	// Try to sync blocks in order
	for height := currentHeight + 1; ; height++ {
		header, hasHeader := s.pendingHeaders[height]
		data, hasData := s.pendingData[height]

		if !hasHeader || !hasData {
			break
		}

		ctx.Logger().Info("syncing block", "height", height)

		// Validate block
		validateReply := make(chan error, 1)
		err := ctx.Tell(s.validatorActor, ValidateBlock{
			Header: header,
			Data:   data,
			Reply:  validateReply,
		})
		if err != nil {
			ctx.Logger().Error("failed to send validate message", "error", err)
			break
		}

		if err := <-validateReply; err != nil {
			ctx.Logger().Error("block validation failed",
				"height", height,
				"error", err)
			// Remove invalid block
			delete(s.pendingHeaders, height)
			delete(s.pendingData, height)
			break
		}

		// Apply block
		result := s.applyBlock(ctx, header, data)
		if result.Error != nil {
			ctx.Logger().Error("failed to apply block",
				"height", height,
				"error", result.Error)
			break
		}

		// Save block
		if err := s.store.SaveBlockData(context.Background(), header, data, &header.Signature); err != nil {
			ctx.Logger().Error("failed to save block",
				"height", height,
				"error", err)
			break
		}

		// Update store height
		if err := s.store.SetHeight(context.Background(), height); err != nil {
			ctx.Logger().Error("failed to update height", "error", err)
			break
		}

		// Update state
		updateReply := make(chan error, 1)
		err = ctx.Tell(s.stateActor, UpdateState{
			State: result.NewState,
			Reply: updateReply,
		})
		if err != nil {
			ctx.Logger().Error("failed to send state update", "error", err)
			break
		}

		if err := <-updateReply; err != nil {
			ctx.Logger().Error("failed to update state", "error", err)
			break
		}

		// Clean up
		delete(s.pendingHeaders, height)
		delete(s.pendingData, height)
		s.headerCache.SetSeen(header.Hash().String())
		s.dataCache.SetSeen(data.DACommitment().String())

		ctx.Logger().Info("block synced successfully", "height", height)
	}
}

// applyBlock executes the block and returns the new state
func (s *SyncActor) applyBlock(ctx actor.Context, header *types.SignedHeader, data *types.Data) *ApplyBlockResult {
	// Get current state
	stateReply := make(chan types.State, 1)
	err := ctx.Tell(s.stateActor, GetState{Reply: stateReply})
	if err != nil {
		return &ApplyBlockResult{Error: err}
	}

	currentState := <-stateReply

	// Execute transactions with circuit breaker
	var newStateRoot []byte
	execErr := s.execCircuitBreaker.Call(func() error {
		rawTxs := make([][]byte, len(data.Txs))
		for i, tx := range data.Txs {
			rawTxs[i] = tx
		}

		var err error
		newStateRoot, _, err = s.exec.ExecuteTxs(
			context.Background(),
			rawTxs,
			header.Height(),
			header.Time(),
			currentState.AppHash,
		)
		return err
	})

	if execErr != nil {
		return &ApplyBlockResult{
			Error: fmt.Errorf("failed to execute transactions: %w", execErr),
		}
	}

	// Create new state
	newState, err := currentState.NextState(header, newStateRoot)
	if err != nil {
		return &ApplyBlockResult{
			Error: fmt.Errorf("failed to create next state: %w", err),
		}
	}

	return &ApplyBlockResult{
		NewState: newState,
		Error:    nil,
	}
}

// getCacheEntry retrieves a cache entry
func (s *SyncActor) getCacheEntry(key string) any {
	// Implement cache lookup logic
	return nil
}

// setCacheEntry sets a cache entry
func (s *SyncActor) setCacheEntry(key string, value any) {
	// Implement cache set logic
}

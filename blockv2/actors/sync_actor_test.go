package actors_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	"github.com/rollkit/rollkit/blockv2/actor"
	"github.com/rollkit/rollkit/blockv2/actors"
	coreexecution "github.com/rollkit/rollkit/core/execution"
	"github.com/rollkit/rollkit/pkg/genesis"
	"github.com/rollkit/rollkit/types"
)

func TestSyncActor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create dependencies
	mockStore := &mockStore{
		height: 10,
		blocks: make(map[uint64]*types.SignedHeader),
		data:   make(map[uint64]*types.Data),
	}
	mockExec := &mockExecutor{}
	gen := genesis.Genesis{
		ChainID:       "test-chain",
		InitialHeight: 1,
	}

	// Create mock state and validator actors
	stateActor := &mockStateActor{
		state: types.State{
			ChainID:         "test-chain",
			LastBlockHeight: 10,
			AppHash:         []byte("current-hash"),
		},
	}
	statePID, err := system.Spawn("state", stateActor)
	require.NoError(t, err)

	validatorActor := &mockValidatorActor{}
	validatorPID, err := system.Spawn("validator", validatorActor)
	require.NoError(t, err)

	// Create sync actor
	syncActor := actors.NewSyncActor(mockStore, mockExec, gen, statePID, validatorPID)
	syncPID, err := system.Spawn("sync", syncActor)
	require.NoError(t, err)

	t.Run("ApplyBlock", func(t *testing.T) {
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  11,
				},
				AppHash: []byte("next-hash"),
			},
		}
		data := &types.Data{
			Txs: []types.Tx{[]byte("tx1")},
		}

		reply := make(chan *actors.ApplyBlockResult, 1)
		err := syncPID.Tell(actors.ApplyBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.NoError(t, result.Error)
			assert.NotNil(t, result.NewState)
			assert.Equal(t, uint64(11), result.NewState.LastBlockHeight)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for apply block")
		}
	})

	t.Run("SyncBlock", func(t *testing.T) {
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  12,
				},
			},
		}
		data := &types.Data{
			Txs: []types.Tx{[]byte("sync-tx")},
		}

		reply := make(chan error, 1)
		err := syncPID.Tell(actors.SyncBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for sync block")
		}

		// Verify block was stored
		storedHeader, ok := mockStore.blocks[12]
		assert.True(t, ok)
		assert.Equal(t, header, storedHeader)
	})

	t.Run("OutOfOrderBlocks", func(t *testing.T) {
		// Apply block at height 15 (out of order)
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  15,
				},
			},
		}
		data := &types.Data{}

		reply := make(chan *actors.ApplyBlockResult, 1)
		err := syncPID.Tell(actors.ApplyBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			// Should handle out of order gracefully
			assert.NoError(t, result.Error)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for out of order block")
		}
	})

	t.Run("InvalidBlock", func(t *testing.T) {
		// Create invalid header (wrong chain ID)
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "wrong-chain",
					Height:  13,
				},
			},
		}
		data := &types.Data{}

		// Set validator to reject
		validatorActor.shouldReject = true

		reply := make(chan *actors.ApplyBlockResult, 1)
		err := syncPID.Tell(actors.ApplyBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.Error(t, result.Error)
			assert.Contains(t, result.Error.Error(), "validation failed")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for invalid block")
		}

		// Reset validator
		validatorActor.shouldReject = false
	})

	t.Run("ExecutionFailure", func(t *testing.T) {
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  14,
				},
			},
		}
		data := &types.Data{
			Txs: []types.Tx{[]byte("failing-tx")},
		}

		// Set executor to fail
		mockExec.shouldFail = true

		reply := make(chan *actors.ApplyBlockResult, 1)
		err := syncPID.Tell(actors.ApplyBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.Error(t, result.Error)
			assert.Contains(t, result.Error.Error(), "execution failed")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for execution failure")
		}

		// Reset executor
		mockExec.shouldFail = false
	})

	t.Run("CacheOperations", func(t *testing.T) {
		// Set cache entry
		setReply := make(chan error, 1)
		err := syncPID.Tell(actors.SetCacheEntry{
			Key:   "test-key",
			Value: "test-value",
			Reply: setReply,
		})
		assert.NoError(t, err)

		select {
		case err := <-setReply:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for set cache entry")
		}

		// Get cache entry
		getReply := make(chan any, 1)
		err = syncPID.Tell(actors.GetCacheEntry{
			Key:   "test-key",
			Reply: getReply,
		})
		assert.NoError(t, err)

		select {
		case value := <-getReply:
			assert.Equal(t, "test-value", value)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for get cache entry")
		}

		// Get non-existent entry
		getReply2 := make(chan any, 1)
		err = syncPID.Tell(actors.GetCacheEntry{
			Key:   "non-existent",
			Reply: getReply2,
		})
		assert.NoError(t, err)

		select {
		case value := <-getReply2:
			assert.Nil(t, value)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for non-existent cache entry")
		}
	})
}

// Mock implementations

type mockExecutor struct {
	shouldFail bool
}

func (m *mockExecutor) InitChain(ctx context.Context, genesisTime time.Time, initialHeight uint64, chainID string) ([]byte, uint64, error) {
	return []byte("genesis-hash"), 0, nil
}

func (m *mockExecutor) GetTxs(ctx context.Context) ([][]byte, error) {
	return [][]byte{}, nil
}

func (m *mockExecutor) ExecuteTxs(ctx context.Context, txs [][]byte, blockHeight uint64, timestamp time.Time, prevStateRoot []byte) ([]byte, uint64, error) {
	if m.shouldFail {
		return nil, 0, assert.AnError
	}
	return []byte("new-state-root"), 1000, nil
}

func (m *mockExecutor) SetFinal(ctx context.Context, blockHeight uint64) error {
	return nil
}

// Enhanced mock state actor with update tracking
type mockStateActorWithUpdates struct {
	state   types.State
	updates []types.State
}

func (m *mockStateActorWithUpdates) Receive(ctx actor.Context, msg any) {
	switch msg := msg.(type) {
	case actors.GetState:
		select {
		case msg.Reply <- m.state:
		default:
		}
	case actors.UpdateState:
		m.state = msg.State
		m.updates = append(m.updates, msg.State)
		select {
		case msg.Reply <- nil:
		default:
		}
	}
}

// Enhanced mock validator actor
type mockValidatorActorEnhanced struct {
	shouldReject bool
	validated    []*types.SignedHeader
}

func (m *mockValidatorActorEnhanced) Receive(ctx actor.Context, msg any) {
	switch msg := msg.(type) {
	case actors.ValidateBlock:
		m.validated = append(m.validated, msg.Header)
		var err error
		if m.shouldReject {
			err = assert.AnError
		}
		select {
		case msg.Reply <- err:
		default:
		}
	}
}
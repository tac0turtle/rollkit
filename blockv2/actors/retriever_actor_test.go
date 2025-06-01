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
	coreda "github.com/rollkit/rollkit/core/da"
	"github.com/rollkit/rollkit/types"
)

func TestRetrieverActor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create mock DA and sync actor
	mockDA := &mockDARetriever{
		data: make(map[string][]byte),
	}
	syncActor := &mockSyncActorForRetriever{}
	syncPID, err := system.Spawn("sync", syncActor)
	require.NoError(t, err)

	// Create retriever actor
	retriever := actors.NewRetrieverActor(
		mockDA,
		"test-chain",
		[]byte("proposer"),
		5, // startHeight
		syncPID,
	)
	retrieverPID, err := system.Spawn("retriever", retriever)
	require.NoError(t, err)

	t.Run("RetrieveData", func(t *testing.T) {
		// Add test data to mock DA
		testData := []byte("test-block-data")
		testID := "block-123"
		mockDA.data[testID] = testData

		reply := make(chan *actors.RetrieveResult, 1)
		err := retrieverPID.Tell(actors.RetrieveData{
			IDs:   []coreda.ID{coreda.ID(testID)},
			Reply: reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.NoError(t, result.Error)
			assert.Len(t, result.Data, 1)
			assert.Equal(t, testData, []byte(result.Data[0]))
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for retrieve data")
		}
	})

	t.Run("RetrieveNonExistentData", func(t *testing.T) {
		reply := make(chan *actors.RetrieveResult, 1)
		err := retrieverPID.Tell(actors.RetrieveData{
			IDs:   []coreda.ID{"non-existent"},
			Reply: reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.Error(t, result.Error)
			assert.Contains(t, result.Error.Error(), "not found")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for non-existent data error")
		}
	})

	t.Run("ProcessBlocks", func(t *testing.T) {
		// Create test header and data
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  10,
				},
				ProposerAddress: []byte("proposer"),
			},
		}
		data := &types.Data{
			Txs: []types.Tx{[]byte("tx1")},
		}

		reply := make(chan error, 1)
		err := retrieverPID.Tell(actors.ProcessBlocks{
			Headers: []*types.SignedHeader{header},
			Data:    []*types.Data{data},
			Reply:   reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for process blocks")
		}

		// Verify sync actor received the block
		assert.Len(t, syncActor.syncedBlocks, 1)
		assert.Equal(t, header, syncActor.syncedBlocks[0].header)
		assert.Equal(t, data, syncActor.syncedBlocks[0].data)
	})

	t.Run("ProcessInvalidProposer", func(t *testing.T) {
		// Create header with wrong proposer
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  11,
				},
				ProposerAddress: []byte("wrong-proposer"),
			},
		}
		data := &types.Data{}

		reply := make(chan error, 1)
		err := retrieverPID.Tell(actors.ProcessBlocks{
			Headers: []*types.SignedHeader{header},
			Data:    []*types.Data{data},
			Reply:   reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "proposer mismatch")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for invalid proposer error")
		}
	})

	t.Run("ProcessWrongChain", func(t *testing.T) {
		// Create header with wrong chain ID
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "wrong-chain",
					Height:  12,
				},
				ProposerAddress: []byte("proposer"),
			},
		}
		data := &types.Data{}

		reply := make(chan error, 1)
		err := retrieverPID.Tell(actors.ProcessBlocks{
			Headers: []*types.SignedHeader{header},
			Data:    []*types.Data{data},
			Reply:   reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "chain ID mismatch")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for wrong chain error")
		}
	})

	t.Run("UpdateDAHeight", func(t *testing.T) {
		reply := make(chan error, 1)
		err := retrieverPID.Tell(actors.UpdateDAHeight{
			Height: 20,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for update DA height")
		}
	})

	t.Run("CircuitBreakerTriggering", func(t *testing.T) {
		// Set DA to always fail
		mockDA.shouldFail = true

		// Make multiple requests to trigger circuit breaker
		for i := 0; i < 5; i++ {
			reply := make(chan *actors.RetrieveResult, 1)
			err := retrieverPID.Tell(actors.RetrieveData{
				IDs:   []coreda.ID{"failing-id"},
				Reply: reply,
			})
			assert.NoError(t, err)

			select {
			case result := <-reply:
				assert.Error(t, result.Error)
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for circuit breaker test")
			}
		}

		// Reset DA
		mockDA.shouldFail = false

		// Circuit breaker should be open, so requests should fail fast
		reply := make(chan *actors.RetrieveResult, 1)
		start := time.Now()
		err = retrieverPID.Tell(actors.RetrieveData{
			IDs:   []coreda.ID{"test-after-cb"},
			Reply: reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			elapsed := time.Since(start)
			assert.Error(t, result.Error)
			// Should fail fast due to circuit breaker
			assert.Less(t, elapsed, 100*time.Millisecond)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for circuit breaker fast fail")
		}
	})
}

// Mock implementations

type mockDARetriever struct {
	data       map[string][]byte
	shouldFail bool
}

func (m *mockDARetriever) Submit(ctx context.Context, blobs []coreda.Blob, gasPrice float64, namespace coreda.Namespace) coreda.ResultSubmit {
	return coreda.ResultSubmit{}
}

func (m *mockDARetriever) Get(ctx context.Context, ids []coreda.ID, namespace coreda.Namespace) coreda.ResultGet {
	if m.shouldFail {
		return coreda.ResultGet{
			Code: coreda.StatusError,
		}
	}

	var blobs []coreda.Blob
	for _, id := range ids {
		data, exists := m.data[string(id)]
		if !exists {
			return coreda.ResultGet{
				Code: coreda.StatusNotFound,
			}
		}
		blobs = append(blobs, coreda.Blob(data))
	}

	return coreda.ResultGet{
		Code:  coreda.StatusSuccess,
		Blobs: blobs,
	}
}

func (m *mockDARetriever) GetProofs(ctx context.Context, ids []coreda.ID, namespace coreda.Namespace) coreda.ResultGetProofs {
	return coreda.ResultGetProofs{}
}

func (m *mockDARetriever) Commit(ctx context.Context, blocks []coreda.Block, namespace coreda.Namespace) coreda.ResultCommit {
	return coreda.ResultCommit{}
}

func (m *mockDARetriever) Validate(ctx context.Context, ids []coreda.ID, proofs []coreda.Proof, namespace coreda.Namespace) coreda.ResultValidate {
	return coreda.ResultValidate{}
}

type syncedBlock struct {
	header *types.SignedHeader
	data   *types.Data
}

type mockSyncActorForRetriever struct {
	syncedBlocks []syncedBlock
}

func (m *mockSyncActorForRetriever) Receive(ctx actor.Context, msg any) {
	switch msg := msg.(type) {
	case actors.SyncBlock:
		m.syncedBlocks = append(m.syncedBlocks, syncedBlock{
			header: msg.Header,
			data:   msg.Data,
		})
		select {
		case msg.Reply <- nil:
		default:
		}
	}
}
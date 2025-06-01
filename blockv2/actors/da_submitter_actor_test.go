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
	coresequencer "github.com/rollkit/rollkit/core/sequencer"
	"github.com/rollkit/rollkit/types"
)

func TestDASubmitterActor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create mock DA includer
	daIncluder := &mockDAIncluderActor{}
	daIncluderPID, err := system.Spawn("da-includer", daIncluder)
	require.NoError(t, err)

	// Create mock DA and store
	mockDA := &mockDA{
		submitResults: make(map[string]coreda.ResultSubmit),
	}
	mockStore := &mockStore{
		height:   10,
		blocks:   make(map[uint64]*types.SignedHeader),
		data:     make(map[uint64]*types.Data),
		metadata: make(map[string][]byte),
	}

	// Add some test blocks
	for i := uint64(1); i <= 10; i++ {
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					Height: i,
				},
			},
		}
		mockStore.blocks[i] = header
		mockStore.data[i] = &types.Data{}
	}

	// Create submitter actor
	submitter := actors.NewDASubmitterActor(
		mockDA,
		mockStore,
		1.0,  // gasPrice
		1.5,  // gasMultiplier
		time.Second, // blockTime
		3,    // mempoolTTL
		daIncluderPID,
	)
	submitterPID, err := system.Spawn("submitter", submitter)
	require.NoError(t, err)

	t.Run("SubmitHeaders", func(t *testing.T) {
		headers := []*types.SignedHeader{
			{
				Header: types.Header{
					BaseHeader: types.BaseHeader{Height: 11},
				},
			},
			{
				Header: types.Header{
					BaseHeader: types.BaseHeader{Height: 12},
				},
			},
		}

		// Set DA to succeed
		mockDA.defaultResult = coreda.ResultSubmit{
			Code:           coreda.StatusSuccess,
			SubmittedCount: 2,
			Height:         100,
		}

		reply := make(chan error, 1)
		err := submitterPID.Tell(actors.SubmitHeaders{
			Headers: headers,
			Reply:   reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for submit headers")
		}

		// Give time for processing
		time.Sleep(100 * time.Millisecond)

		// Check DA includer received notifications
		assert.Len(t, daIncluder.included, 2)
		assert.Equal(t, uint64(11), daIncluder.included[0].Height)
		assert.Equal(t, uint64(12), daIncluder.included[1].Height)
	})

	t.Run("SubmitBatch", func(t *testing.T) {
		batch := coresequencer.Batch{
			Transactions: [][]byte{
				[]byte("tx1"),
				[]byte("tx2"),
			},
		}

		// Set DA to succeed
		mockDA.defaultResult = coreda.ResultSubmit{
			Code:   coreda.StatusSuccess,
			Height: 101,
		}

		reply := make(chan error, 1)
		err := submitterPID.Tell(actors.SubmitBatch{
			Batch: batch,
			Reply: reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for submit batch")
		}

		// Give time for processing
		time.Sleep(100 * time.Millisecond)

		// Check submission occurred
		assert.True(t, mockDA.submitCalled)
	})

	t.Run("SubmitWithFailure", func(t *testing.T) {
		headers := []*types.SignedHeader{
			{
				Header: types.Header{
					BaseHeader: types.BaseHeader{Height: 13},
				},
			},
		}

		// Set DA to fail
		mockDA.defaultResult = coreda.ResultSubmit{
			Code: coreda.StatusNotIncludedInBlock,
		}

		reply := make(chan error, 1)
		err := submitterPID.Tell(actors.SubmitHeaders{
			Headers: headers,
			Reply:   reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err) // Should still reply without error immediately
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for submit headers")
		}
	})

	t.Run("BatchQueueFull", func(t *testing.T) {
		// Fill the queue
		for i := 0; i < 100; i++ {
			batch := coresequencer.Batch{
				Transactions: [][]byte{[]byte("tx")},
			}
			submitterPID.Tell(actors.SubmitBatch{
				Batch: batch,
				Reply: make(chan error, 1),
			})
		}

		// Try to add one more
		batch := coresequencer.Batch{
			Transactions: [][]byte{[]byte("overflow-tx")},
		}
		reply := make(chan error, 1)
		err := submitterPID.Tell(actors.SubmitBatch{
			Batch: batch,
			Reply: reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "queue full")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for queue full error")
		}
	})
}

// Mock implementations

type mockDA struct {
	submitCalled  bool
	defaultResult coreda.ResultSubmit
	submitResults map[string]coreda.ResultSubmit
}

func (m *mockDA) Submit(ctx context.Context, blobs []coreda.Blob, gasPrice float64, namespace coreda.Namespace) coreda.ResultSubmit {
	m.submitCalled = true
	return m.defaultResult
}

func (m *mockDA) Get(ctx context.Context, ids []coreda.ID, namespace coreda.Namespace) coreda.ResultGet {
	return coreda.ResultGet{}
}

func (m *mockDA) GetProofs(ctx context.Context, ids []coreda.ID, namespace coreda.Namespace) coreda.ResultGetProofs {
	return coreda.ResultGetProofs{}
}

func (m *mockDA) Commit(ctx context.Context, blocks []coreda.Block, namespace coreda.Namespace) coreda.ResultCommit {
	return coreda.ResultCommit{}
}

func (m *mockDA) Validate(ctx context.Context, ids []coreda.ID, proofs []coreda.Proof, namespace coreda.Namespace) coreda.ResultValidate {
	return coreda.ResultValidate{}
}

type mockDAIncluderActor struct {
	included []actors.DAIncluded
}

func (m *mockDAIncluderActor) Receive(ctx actor.Context, msg any) {
	switch msg := msg.(type) {
	case actors.DAIncluded:
		m.included = append(m.included, msg)
	}
}
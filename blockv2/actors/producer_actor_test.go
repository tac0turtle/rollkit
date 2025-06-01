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
	coresequencer "github.com/rollkit/rollkit/core/sequencer"
	"github.com/rollkit/rollkit/pkg/genesis"
	"github.com/rollkit/rollkit/pkg/store"
	"github.com/rollkit/rollkit/types"
)

func TestProducerActor(t *testing.T) {
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
	mockSigner := &mockSigner{}
	mockSequencer := &mockSequencer{}
	gen := genesis.Genesis{
		ChainID:         "test-chain",
		InitialHeight:   1,
		ProposerAddress: []byte("proposer"),
	}

	// Create mock actors
	stateActor := &mockStateActor{
		state: types.State{
			ChainID:         "test-chain",
			LastBlockHeight: 10,
			LastBlockTime:   time.Now().Add(-time.Hour),
			AppHash:         []byte("app-hash"),
		},
	}
	statePID, err := system.Spawn("state", stateActor)
	require.NoError(t, err)

	syncActor := &mockSyncActor{}
	syncPID, err := system.Spawn("sync", syncActor)
	require.NoError(t, err)

	validatorActor := &mockValidatorActor{}
	_, err = system.Spawn("validator", validatorActor)
	require.NoError(t, err)

	// Create producer actor
	producer := actors.NewProducerActor(
		mockStore,
		mockSigner,
		gen,
		mockSequencer,
		100, // maxPendingHeaders
		statePID,
		nil, // sequencerActor
		syncPID,
		nil, // submitterActor
	)
	producerPID, err := system.Spawn("producer", producer)
	require.NoError(t, err)

	t.Run("ProduceBlock", func(t *testing.T) {
		reply := make(chan *actors.ProduceBlockResult, 1)
		err := producerPID.Tell(actors.ProduceBlock{
			Timestamp: time.Now(),
			Reply:     reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.NoError(t, result.Error)
			assert.NotNil(t, result.Header)
			assert.NotNil(t, result.Data)
			assert.Equal(t, uint64(11), result.Header.Height())
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for block production")
		}
	})

	t.Run("ProduceEmptyBlock", func(t *testing.T) {
		// Set sequencer to return no batch
		mockSequencer.noBatch = true

		reply := make(chan *actors.ProduceBlockResult, 1)
		err := producerPID.Tell(actors.ProduceBlock{
			Timestamp: time.Now(),
			Reply:     reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.NoError(t, result.Error)
			assert.NotNil(t, result.Header)
			assert.NotNil(t, result.Data)
			assert.Empty(t, result.Data.Txs)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for empty block production")
		}
	})

	t.Run("ExistingBlock", func(t *testing.T) {
		// Add existing block at height 12
		existingHeader := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  12,
				},
			},
		}
		mockStore.blocks[12] = existingHeader
		mockStore.data[12] = &types.Data{}

		reply := make(chan *actors.ProduceBlockResult, 1)
		err := producerPID.Tell(actors.ProduceBlock{
			Timestamp: time.Now(),
			Reply:     reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.NoError(t, result.Error)
			assert.Equal(t, existingHeader, result.Header)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for existing block")
		}
	})

	t.Run("PendingHeadersLimit", func(t *testing.T) {
		// Update pending headers count to exceed limit
		err := producerPID.Tell(&actors.UpdatePendingHeaders{Count: 101})
		assert.NoError(t, err)

		time.Sleep(10 * time.Millisecond) // Give time for message processing

		reply := make(chan *actors.ProduceBlockResult, 1)
		err = producerPID.Tell(actors.ProduceBlock{
			Timestamp: time.Now(),
			Reply:     reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.Error(t, result.Error)
			assert.Contains(t, result.Error.Error(), "pending headers limit reached")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for pending headers error")
		}
	})
}

// Mock implementations

type mockStore struct {
	height          uint64
	blocks          map[uint64]*types.SignedHeader
	data            map[uint64]*types.Data
	metadata        map[string][]byte
	state           types.State
	saveStateCalled bool
}

func (m *mockStore) Height(ctx context.Context) (uint64, error) {
	return m.height, nil
}

func (m *mockStore) SetHeight(ctx context.Context, height uint64) error {
	m.height = height
	return nil
}

func (m *mockStore) GetBlockData(ctx context.Context, height uint64) (*types.SignedHeader, *types.Data, error) {
	header, ok := m.blocks[height]
	if !ok {
		return nil, nil, store.ErrNotFound
	}
	data := m.data[height]
	return header, data, nil
}

func (m *mockStore) SaveBlockData(ctx context.Context, header *types.SignedHeader, data *types.Data, signature *types.Signature) error {
	m.blocks[header.Height()] = header
	m.data[header.Height()] = data
	return nil
}

func (m *mockStore) GetSignature(ctx context.Context, height uint64) (*types.Signature, error) {
	return &types.Signature{}, nil
}

func (m *mockStore) GetMetadata(ctx context.Context, key string) ([]byte, error) {
	if m.metadata == nil {
		m.metadata = make(map[string][]byte)
	}
	data, ok := m.metadata[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return data, nil
}

func (m *mockStore) SetMetadata(ctx context.Context, key string, value []byte) error {
	if m.metadata == nil {
		m.metadata = make(map[string][]byte)
	}
	m.metadata[key] = value
	return nil
}

func (m *mockStore) GetState(ctx context.Context) (types.State, error) {
	if (m.state == types.State{}) {
		return types.State{}, store.ErrNotFound
	}
	return m.state, nil
}

func (m *mockStore) SaveState(ctx context.Context, state types.State) error {
	m.saveStateCalled = true
	m.state = state
	return nil
}

type mockSigner struct{}

func (m *mockSigner) GetPublic() (any, error) {
	return &mockPubKey{}, nil
}

func (m *mockSigner) GetAddress() ([]byte, error) {
	return []byte("proposer"), nil
}

func (m *mockSigner) Sign(data []byte) (types.Signature, error) {
	return []byte("signature"), nil
}

type mockSequencer struct {
	noBatch bool
}

func (m *mockSequencer) GetNextBatch(ctx context.Context, req coresequencer.GetNextBatchRequest) (*coresequencer.GetNextBatchResponse, error) {
	if m.noBatch {
		return &coresequencer.GetNextBatchResponse{
			Batch: &coresequencer.Batch{
				Transactions: [][]byte{},
			},
		}, nil
	}

	return &coresequencer.GetNextBatchResponse{
		Batch: &coresequencer.Batch{
			Transactions: [][]byte{
				[]byte("tx1"),
				[]byte("tx2"),
			},
		},
		Timestamp: time.Now(),
		BatchData: [][]byte{[]byte("batch-data")},
	}, nil
}

type mockSyncActor struct{}

func (m *mockSyncActor) Receive(ctx actor.Context, msg any) {
	switch msg := msg.(type) {
	case actors.ApplyBlock:
		select {
		case msg.Reply <- &actors.ApplyBlockResult{
			NewState: types.State{
				ChainID:         "test-chain",
				LastBlockHeight: msg.Header.Height(),
				AppHash:         []byte("new-app-hash"),
			},
		}:
		default:
		}
	}
}

type mockValidatorActor struct{}

func (m *mockValidatorActor) Receive(ctx actor.Context, msg any) {
	switch msg := msg.(type) {
	case actors.ValidateBlock:
		select {
		case msg.Reply <- nil: // Valid block
		default:
		}
	}
}

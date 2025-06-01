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
	"github.com/rollkit/rollkit/types"
)

func TestStateActor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	initialState := types.State{
		ChainID:         "test-chain",
		LastBlockHeight: 10,
		LastBlockTime:   time.Now().Add(-time.Hour),
		AppHash:         []byte("initial-app-hash"),
		DAHeight:        5,
	}

	mockStore := &mockStore{
		height: 10,
		blocks: make(map[uint64]*types.SignedHeader),
		data:   make(map[uint64]*types.Data),
	}

	// Create state actor
	stateActor := actors.NewStateActor(mockStore, initialState)
	statePID, err := system.Spawn("state", stateActor)
	require.NoError(t, err)

	t.Run("GetState", func(t *testing.T) {
		reply := make(chan types.State, 1)
		err := statePID.Tell(actors.GetState{Reply: reply})
		assert.NoError(t, err)

		select {
		case state := <-reply:
			assert.Equal(t, "test-chain", state.ChainID)
			assert.Equal(t, uint64(10), state.LastBlockHeight)
			assert.Equal(t, []byte("initial-app-hash"), state.AppHash)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for get state")
		}
	})

	t.Run("UpdateState", func(t *testing.T) {
		newState := types.State{
			ChainID:         "test-chain",
			LastBlockHeight: 11,
			LastBlockTime:   time.Now(),
			AppHash:         []byte("new-app-hash"),
			DAHeight:        6,
		}

		reply := make(chan error, 1)
		err := statePID.Tell(actors.UpdateState{
			State: newState,
			Reply: reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for update state")
		}

		// Verify state was updated
		getReply := make(chan types.State, 1)
		err = statePID.Tell(actors.GetState{Reply: getReply})
		assert.NoError(t, err)

		select {
		case state := <-getReply:
			assert.Equal(t, uint64(11), state.LastBlockHeight)
			assert.Equal(t, []byte("new-app-hash"), state.AppHash)
			assert.Equal(t, uint64(6), state.DAHeight)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for updated state")
		}
	})

	t.Run("GetHeight", func(t *testing.T) {
		reply := make(chan uint64, 1)
		err := statePID.Tell(actors.GetHeight{Reply: reply})
		assert.NoError(t, err)

		select {
		case height := <-reply:
			assert.Equal(t, uint64(11), height)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for get height")
		}
	})

	t.Run("UpdateDAHeight", func(t *testing.T) {
		reply := make(chan error, 1)
		err := statePID.Tell(actors.UpdateDAHeight{
			Height: 8,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for update DA height")
		}

		// Verify DA height was updated
		getReply := make(chan types.State, 1)
		err = statePID.Tell(actors.GetState{Reply: getReply})
		assert.NoError(t, err)

		select {
		case state := <-getReply:
			assert.Equal(t, uint64(8), state.DAHeight)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for state with updated DA height")
		}
	})

	t.Run("ConcurrentStateUpdates", func(t *testing.T) {
		// Test concurrent state updates to ensure thread safety
		const numUpdates = 100
		errChan := make(chan error, numUpdates)

		for i := 0; i < numUpdates; i++ {
			go func(i int) {
				state := types.State{
					ChainID:         "test-chain",
					LastBlockHeight: uint64(11 + i),
					AppHash:         []byte("concurrent-hash"),
				}

				reply := make(chan error, 1)
				err := statePID.Tell(actors.UpdateState{
					State: state,
					Reply: reply,
				})
				if err != nil {
					errChan <- err
					return
				}

				select {
				case err := <-reply:
					errChan <- err
				case <-time.After(time.Second):
					errChan <- assert.AnError
				}
			}(i)
		}

		// Collect all errors
		for i := 0; i < numUpdates; i++ {
			select {
			case err := <-errChan:
				assert.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("timeout waiting for concurrent updates")
			}
		}

		// Verify final state
		getReply := make(chan types.State, 1)
		err = statePID.Tell(actors.GetState{Reply: getReply})
		assert.NoError(t, err)

		select {
		case state := <-getReply:
			// Height should be at least 11 (updates are concurrent so exact value varies)
			assert.GreaterOrEqual(t, state.LastBlockHeight, uint64(11))
			assert.Equal(t, []byte("concurrent-hash"), state.AppHash)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for final state")
		}
	})

	t.Run("InvalidHeightUpdate", func(t *testing.T) {
		// Try to update to a lower height (should still work as it's just state storage)
		state := types.State{
			ChainID:         "test-chain",
			LastBlockHeight: 5, // Lower than current
			AppHash:         []byte("lower-hash"),
		}

		reply := make(chan error, 1)
		err := statePID.Tell(actors.UpdateState{
			State: state,
			Reply: reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err) // State actor just stores what it's given
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for lower height update")
		}
	})
}

func TestStateActorPersistence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	initialState := types.State{
		ChainID:         "test-chain",
		LastBlockHeight: 10,
		AppHash:         []byte("initial-hash"),
	}

	mockStore := &mockStore{
		height: 10,
		blocks: make(map[uint64]*types.SignedHeader),
		data:   make(map[uint64]*types.Data),
	}

	// Create and update state
	stateActor := actors.NewStateActor(mockStore, initialState)
	statePID, err := system.Spawn("state", stateActor)
	require.NoError(t, err)

	newState := types.State{
		ChainID:         "test-chain",
		LastBlockHeight: 15,
		AppHash:         []byte("persisted-hash"),
	}

	reply := make(chan error, 1)
	err = statePID.Tell(actors.UpdateState{
		State: newState,
		Reply: reply,
	})
	assert.NoError(t, err)

	select {
	case err := <-reply:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for state update")
	}

	// Verify store was updated
	assert.True(t, mockStore.saveStateCalled)

	// Create new actor with same store to test persistence
	stateActor2 := actors.NewStateActor(mockStore, initialState)
	statePID2, err := system.Spawn("state2", stateActor2)
	require.NoError(t, err)

	// Should have the updated state
	getReply := make(chan types.State, 1)
	err = statePID2.Tell(actors.GetState{Reply: getReply})
	assert.NoError(t, err)

	select {
	case state := <-getReply:
		assert.Equal(t, uint64(15), state.LastBlockHeight)
		assert.Equal(t, []byte("persisted-hash"), state.AppHash)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for persisted state")
	}
}
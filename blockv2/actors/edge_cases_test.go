package actors_test

import (
	"context"
	"sync"
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

// TestEdgeCasesFromBlockV1 tests critical edge cases identified from /block tests
func TestEdgeCasesFromBlockV1(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	t.Run("TimerPrecisionAndTolerance", func(t *testing.T) {
		// Test timer precision similar to aggregation_test.go
		cfg := mockConfig{blockTime: 100 * time.Millisecond}
		gen := mockGenesis()
		
		producerActor := &mockProducerActor{}
		producerPID, err := system.Spawn("producer", producerActor)
		require.NoError(t, err)
		
		stateActor := &mockStateActor{state: types.State{ChainID: "test"}}
		statePID, err := system.Spawn("state", stateActor)
		require.NoError(t, err)

		aggregator := actors.NewAggregatorActor(cfg, gen, producerPID, statePID)
		aggregatorPID, err := system.Spawn("aggregator", aggregator)
		require.NoError(t, err)

		// Measure block production timing
		start := time.Now()
		
		// Trigger multiple rapid productions
		for i := 0; i < 5; i++ {
			err := aggregatorPID.Tell(actors.TxAvailable{})
			assert.NoError(t, err)
		}

		// Wait for processing
		time.Sleep(300 * time.Millisecond)
		elapsed := time.Since(start)

		// Should handle rapid triggers without timing issues
		assert.Less(t, elapsed, 500*time.Millisecond)
		assert.Greater(t, producerActor.produceCount, 0)
	})

	t.Run("OutOfOrderBlockProcessing", func(t *testing.T) {
		// Test out-of-order scenarios from sync_test.go
		mockStore := &mockStore{
			height: 10,
			blocks: make(map[uint64]*types.SignedHeader),
			data:   make(map[uint64]*types.Data),
		}
		
		stateActor := &mockStateActor{
			state: types.State{
				ChainID:         "test",
				LastBlockHeight: 10,
			},
		}
		statePID, err := system.Spawn("state", stateActor)
		require.NoError(t, err)

		validatorActor := &mockValidatorActor{}
		validatorPID, err := system.Spawn("validator", validatorActor)
		require.NoError(t, err)

		syncActor := actors.NewSyncActor(mockStore, &mockExecutor{}, mockGenesis(), statePID, validatorPID)
		syncPID, err := system.Spawn("sync", syncActor)
		require.NoError(t, err)

		// Apply blocks out of order (height 15 before 11-14)
		futureHeader := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test",
					Height:  15,
				},
			},
		}

		reply := make(chan *actors.ApplyBlockResult, 1)
		err = syncPID.Tell(actors.ApplyBlock{
			Header: futureHeader,
			Data:   &types.Data{},
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			// Should handle out-of-order gracefully
			assert.NoError(t, result.Error)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for out-of-order block")
		}
	})

	t.Run("DALayerCorruptedData", func(t *testing.T) {
		// Test corrupted DA data scenarios from retriever_test.go
		mockDA := &mockDARetriever{
			data: map[string][]byte{
				"corrupted": []byte("invalid-protobuf-data"),
				"empty":     []byte{},
			},
		}

		syncActor := &mockSyncActorForRetriever{}
		syncPID, err := system.Spawn("sync", syncActor)
		require.NoError(t, err)

		retriever := actors.NewRetrieverActor(
			mockDA,
			"test-chain",
			[]byte("proposer"),
			0,
			syncPID,
		)
		retrieverPID, err := system.Spawn("retriever", retriever)
		require.NoError(t, err)

		// Test corrupted data
		reply := make(chan *actors.RetrieveResult, 1)
		err = retrieverPID.Tell(actors.RetrieveData{
			IDs:   []coreda.ID{"corrupted"},
			Reply: reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.Error(t, result.Error)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for corrupted data error")
		}

		// Test empty data
		reply2 := make(chan *actors.RetrieveResult, 1)
		err = retrieverPID.Tell(actors.RetrieveData{
			IDs:   []coreda.ID{"empty"},
			Reply: reply2,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply2:
			assert.Error(t, result.Error)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for empty data error")
		}
	})

	t.Run("ResourceExhaustionScenarios", func(t *testing.T) {
		// Test resource exhaustion from various block tests
		
		// Small mailbox system for testing overflow
		smallSystem := actor.NewSystem(ctx, logger, actor.WithMailboxSize(5))
		
		slowActor := &slowProcessingActor{processTime: 100 * time.Millisecond}
		slowPID, err := smallSystem.Spawn("slow", slowActor, actor.WithCustomMailboxSize(3))
		require.NoError(t, err)

		// Flood with messages to trigger overflow
		var failCount int
		for i := 0; i < 20; i++ {
			if err := slowPID.Tell("message"); err != nil {
				failCount++
			}
		}

		// Should have some failures due to mailbox overflow
		assert.Greater(t, failCount, 0)
	})

	t.Run("EmptyBlockSpecialHandling", func(t *testing.T) {
		// Test empty block scenarios from sync_test.go
		mockStore := &mockStore{
			height: 10,
			blocks: make(map[uint64]*types.SignedHeader),
			data:   make(map[uint64]*types.Data),
		}

		stateActor := &mockStateActor{
			state: types.State{
				ChainID:         "test",
				LastBlockHeight: 10,
			},
		}
		statePID, err := system.Spawn("state", stateActor)
		require.NoError(t, err)

		// Test producer with empty batch
		mockSequencer := &mockSequencer{noBatch: true}
		producer := actors.NewProducerActor(
			mockStore,
			&mockSigner{},
			mockGenesis(),
			mockSequencer,
			100,
			statePID,
			nil, nil, nil,
		)
		producerPID, err := system.Spawn("producer", producer)
		require.NoError(t, err)

		reply := make(chan *actors.ProduceBlockResult, 1)
		err = producerPID.Tell(actors.ProduceBlock{
			Timestamp: time.Now(),
			Reply:     reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.NoError(t, result.Error)
			assert.Empty(t, result.Data.Txs)
			// Should handle empty blocks correctly
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for empty block")
		}
	})

	t.Run("ConcurrentStateUpdates", func(t *testing.T) {
		// Test concurrent state updates from various tests
		stateActor := actors.NewStateActor(&mockStore{}, types.State{ChainID: "test"})
		statePID, err := system.Spawn("concurrent-state", stateActor)
		require.NoError(t, err)

		const numConcurrent = 50
		var wg sync.WaitGroup
		errors := make(chan error, numConcurrent)

		// Concurrent state updates
		for i := 0; i < numConcurrent; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				
				reply := make(chan error, 1)
				err := statePID.Tell(actors.UpdateState{
					State: types.State{
						ChainID:         "test",
						LastBlockHeight: uint64(100 + i),
					},
					Reply: reply,
				})
				if err != nil {
					errors <- err
					return
				}

				select {
				case err := <-reply:
					errors <- err
				case <-time.After(time.Second):
					errors <- assert.AnError
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		// All updates should succeed without race conditions
		for err := range errors {
			assert.NoError(t, err)
		}
	})

	t.Run("CircuitBreakerErrorRecovery", func(t *testing.T) {
		// Test circuit breaker scenarios from retriever patterns
		mockDA := &mockDARetriever{shouldFail: true}
		
		syncActor := &mockSyncActorForRetriever{}
		syncPID, err := system.Spawn("sync", syncActor)
		require.NoError(t, err)

		retriever := actors.NewRetrieverActor(mockDA, "test", []byte("proposer"), 0, syncPID)
		retrieverPID, err := system.Spawn("retriever", retriever)
		require.NoError(t, err)

		// Trigger circuit breaker with multiple failures
		for i := 0; i < 5; i++ {
			reply := make(chan *actors.RetrieveResult, 1)
			err := retrieverPID.Tell(actors.RetrieveData{
				IDs:   []coreda.ID{"test"},
				Reply: reply,
			})
			assert.NoError(t, err)

			select {
			case <-reply:
				// Expected to fail
			case <-time.After(100 * time.Millisecond):
				t.Fatal("timeout")
			}
		}

		// Reset DA to working state
		mockDA.shouldFail = false

		// Should still fail fast due to circuit breaker
		reply := make(chan *actors.RetrieveResult, 1)
		start := time.Now()
		err = retrieverPID.Tell(actors.RetrieveData{
			IDs:   []coreda.ID{"test"},
			Reply: reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			elapsed := time.Since(start)
			assert.Error(t, result.Error)
			// Should fail fast due to circuit breaker
			assert.Less(t, elapsed, 50*time.Millisecond)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for circuit breaker")
		}
	})

	t.Run("MessageValidationEdgeCases", func(t *testing.T) {
		// Test message validation patterns
		validator := actors.NewValidatorActor("test", []byte("proposer"), nil)
		validatorPID, err := system.Spawn("validator", validator)
		require.NoError(t, err)

		// Test nil header
		reply := make(chan error, 1)
		err = validatorPID.Tell(actors.ValidateBlock{
			Header: nil,
			Data:   &types.Data{},
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.Error(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for nil header validation")
		}

		// Test nil data
		reply2 := make(chan error, 1)
		err = validatorPID.Tell(actors.ValidateBlock{
			Header: &types.SignedHeader{},
			Data:   nil,
			Reply:  reply2,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply2:
			assert.Error(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for nil data validation")
		}
	})
}

// Additional mock types for edge case testing

type mockConfig struct {
	blockTime time.Duration
}

func (m mockConfig) Rollkit() struct {
	DAConfig struct {
		BlockTime struct {
			Duration time.Duration
		}
	}
} {
	return struct {
		DAConfig struct {
			BlockTime struct {
				Duration time.Duration
			}
		}
	}{
		DAConfig: struct {
			BlockTime struct {
				Duration time.Duration
			}
		}{
			BlockTime: struct {
				Duration time.Duration
			}{Duration: m.blockTime},
		},
	}
}

func mockGenesis() struct {
	ChainID string
} {
	return struct {
		ChainID string
	}{ChainID: "test"}
}
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
	"github.com/rollkit/rollkit/pkg/config"
	"github.com/rollkit/rollkit/pkg/genesis"
	"github.com/rollkit/rollkit/types"
)

func TestAggregatorActor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create configuration
	cfg := config.Config{
		Rollkit: config.RollkitConfig{
			DAConfig: config.DAConfig{
				BlockTime: config.Duration{Duration: 100 * time.Millisecond},
			},
		},
	}

	gen := genesis.Genesis{
		ChainID: "test-chain",
	}

	// Create mock producer and state actors
	producerActor := &mockProducerActor{}
	producerPID, err := system.Spawn("producer", producerActor)
	require.NoError(t, err)

	stateActor := &mockStateActor{
		state: types.State{
			ChainID:         "test-chain",
			LastBlockHeight: 10,
		},
	}
	statePID, err := system.Spawn("state", stateActor)
	require.NoError(t, err)

	// Create aggregator actor
	aggregator := actors.NewAggregatorActor(cfg, gen, producerPID, statePID)
	aggregatorPID, err := system.Spawn("aggregator", aggregator)
	require.NoError(t, err)

	t.Run("ManualProduceBlock", func(t *testing.T) {
		reply := make(chan *actors.ProduceBlockResult, 1)
		err := aggregatorPID.Tell(actors.ProduceBlock{
			Timestamp: time.Now(),
			Reply:     reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.NoError(t, result.Error)
			assert.NotNil(t, result.Header)
			assert.Equal(t, uint64(11), result.Header.Height())
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for manual block production")
		}

		// Verify producer was called
		assert.Equal(t, 1, producerActor.produceCount)
	})

	t.Run("TxAvailableTriggersProduction", func(t *testing.T) {
		initialCount := producerActor.produceCount

		err := aggregatorPID.Tell(actors.TxAvailable{})
		assert.NoError(t, err)

		// Give time for processing
		time.Sleep(200 * time.Millisecond)

		// Should have produced a block
		assert.Greater(t, producerActor.produceCount, initialCount)
	})

	t.Run("AutomaticBlockProduction", func(t *testing.T) {
		// Wait for automatic block production (should happen due to timer)
		initialCount := producerActor.produceCount
		
		// Wait longer than block time
		time.Sleep(300 * time.Millisecond)

		// Should have produced at least one more block
		assert.Greater(t, producerActor.produceCount, initialCount)
	})

	t.Run("ConcurrentProduction", func(t *testing.T) {
		// Send multiple concurrent production requests
		const numRequests = 10
		replies := make([]chan *actors.ProduceBlockResult, numRequests)

		for i := 0; i < numRequests; i++ {
			replies[i] = make(chan *actors.ProduceBlockResult, 1)
			err := aggregatorPID.Tell(actors.ProduceBlock{
				Timestamp: time.Now(),
				Reply:     replies[i],
			})
			assert.NoError(t, err)
		}

		// Collect all replies
		successCount := 0
		for i := 0; i < numRequests; i++ {
			select {
			case result := <-replies[i]:
				if result.Error == nil {
					successCount++
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timeout waiting for concurrent production")
			}
		}

		// Should have some successful productions
		assert.Greater(t, successCount, 0)
	})

	t.Run("ProductionWithFailure", func(t *testing.T) {
		// Set producer to fail
		producerActor.shouldFail = true

		reply := make(chan *actors.ProduceBlockResult, 1)
		err := aggregatorPID.Tell(actors.ProduceBlock{
			Timestamp: time.Now(),
			Reply:     reply,
		})
		assert.NoError(t, err)

		select {
		case result := <-reply:
			assert.Error(t, result.Error)
			assert.Contains(t, result.Error.Error(), "production failed")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for production failure")
		}

		// Reset producer
		producerActor.shouldFail = false
	})

	t.Run("HealthCheck", func(t *testing.T) {
		// Send health check
		err := aggregatorPID.Tell(actor.HealthCheck{})
		assert.NoError(t, err)

		// Give time for processing
		time.Sleep(10 * time.Millisecond)

		// Should not crash
		assert.True(t, aggregatorPID.IsRunning())
	})
}

func TestAggregatorActorConfiguration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	t.Run("DifferentBlockTimes", func(t *testing.T) {
		blockTimes := []time.Duration{
			50 * time.Millisecond,
			200 * time.Millisecond,
			500 * time.Millisecond,
		}

		for _, blockTime := range blockTimes {
			cfg := config.Config{
				Rollkit: config.RollkitConfig{
					DAConfig: config.DAConfig{
						BlockTime: config.Duration{Duration: blockTime},
					},
				},
			}

			gen := genesis.Genesis{ChainID: "test-chain"}

			producerActor := &mockProducerActor{}
			producerPID, err := system.Spawn("producer-"+blockTime.String(), producerActor)
			require.NoError(t, err)

			stateActor := &mockStateActor{
				state: types.State{ChainID: "test-chain"},
			}
			statePID, err := system.Spawn("state-"+blockTime.String(), stateActor)
			require.NoError(t, err)

			aggregator := actors.NewAggregatorActor(cfg, gen, producerPID, statePID)
			aggregatorPID, err := system.Spawn("aggregator-"+blockTime.String(), aggregator)
			require.NoError(t, err)

			// Trigger production and measure timing
			start := time.Now()
			reply := make(chan *actors.ProduceBlockResult, 1)
			err = aggregatorPID.Tell(actors.ProduceBlock{
				Timestamp: time.Now(),
				Reply:     reply,
			})
			assert.NoError(t, err)

			select {
			case result := <-reply:
				elapsed := time.Since(start)
				assert.NoError(t, result.Error)
				// Should complete quickly regardless of block time
				assert.Less(t, elapsed, time.Second)
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for production")
			}
		}
	})
}

// Mock implementations

type mockProducerActor struct {
	produceCount int
	shouldFail   bool
}

func (m *mockProducerActor) Receive(ctx actor.Context, msg any) {
	switch msg := msg.(type) {
	case actors.ProduceBlock:
		m.produceCount++
		
		var result *actors.ProduceBlockResult
		if m.shouldFail {
			result = &actors.ProduceBlockResult{
				Error: assert.AnError,
			}
		} else {
			result = &actors.ProduceBlockResult{
				Header: &types.SignedHeader{
					Header: types.Header{
						BaseHeader: types.BaseHeader{
							Height: uint64(10 + m.produceCount),
						},
					},
				},
				Data: &types.Data{},
			}
		}

		select {
		case msg.Reply <- result:
		default:
		}
	}
}
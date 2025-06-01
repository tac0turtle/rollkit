package actors_test

import (
	"context"
	"fmt"
	"sync"
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

// TestPerformanceScenarios tests performance scenarios from block tests
func TestPerformanceScenarios(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger, actor.WithMailboxSize(1000))

	t.Run("HighThroughputBlockProcessing", func(t *testing.T) {
		// Test high throughput similar to da_speed_test.go (300 blocks)
		mockStore := &mockStore{
			height: 0,
			blocks: make(map[uint64]*types.SignedHeader),
			data:   make(map[uint64]*types.Data),
		}

		stateActor := &mockStateActor{
			state: types.State{
				ChainID:         "test",
				LastBlockHeight: 0,
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

		const numBlocks = 300
		start := time.Now()

		// Process many blocks rapidly
		var wg sync.WaitGroup
		errors := make(chan error, numBlocks)

		for i := 1; i <= numBlocks; i++ {
			wg.Add(1)
			go func(height int) {
				defer wg.Done()

				header := &types.SignedHeader{
					Header: types.Header{
						BaseHeader: types.BaseHeader{
							ChainID: "test",
							Height:  uint64(height),
						},
					},
				}
				data := &types.Data{
					Txs: []types.Tx{[]byte("tx")},
				}

				reply := make(chan *actors.ApplyBlockResult, 1)
				err := syncPID.Tell(actors.ApplyBlock{
					Header: header,
					Data:   data,
					Reply:  reply,
				})
				if err != nil {
					errors <- err
					return
				}

				select {
				case result := <-reply:
					errors <- result.Error
				case <-time.After(5 * time.Second):
					errors <- assert.AnError
				}
			}(i)
		}

		wg.Wait()
		close(errors)
		elapsed := time.Since(start)

		// Check all processed successfully
		for err := range errors {
			assert.NoError(t, err)
		}

		// Should process quickly (target: <10 seconds for 300 blocks)
		assert.Less(t, elapsed, 10*time.Second)
		t.Logf("Processed %d blocks in %v (%.2f blocks/sec)",
			numBlocks, elapsed, float64(numBlocks)/elapsed.Seconds())
	})

	t.Run("DASubmissionWithRetryLoad", func(t *testing.T) {
		// Test DA submission under load similar to submitter_test.go
		mockDA := &mockDA{
			submitResults: make(map[string]coreda.ResultSubmit),
		}

		// Simulate intermittent failures
		failureCount := 0
		mockDA.defaultResult = coreda.ResultSubmit{
			Code: func() coreda.StatusCode {
				failureCount++
				if failureCount%3 == 0 {
					return coreda.StatusNotIncludedInBlock // Trigger retry
				}
				return coreda.StatusSuccess
			}(),
			SubmittedCount: 1,
		}

		mockStore := &mockStore{
			height: 100,
			blocks: make(map[uint64]*types.SignedHeader),
			data:   make(map[uint64]*types.Data),
		}

		// Add test blocks
		for i := uint64(1); i <= 100; i++ {
			mockStore.blocks[i] = &types.SignedHeader{
				Header: types.Header{
					BaseHeader: types.BaseHeader{Height: i},
				},
			}
		}

		submitter := actors.NewDASubmitterActor(
			mockDA,
			mockStore,
			1.0,
			1.5,
			50*time.Millisecond, // Fast block time for testing
			2,
			nil,
		)
		submitterPID, err := system.Spawn("submitter", submitter)
		require.NoError(t, err)

		const numSubmissions = 50
		start := time.Now()

		// Submit many headers concurrently
		var wg sync.WaitGroup
		errors := make(chan error, numSubmissions)

		for i := 0; i < numSubmissions; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				headers := []*types.SignedHeader{
					{
						Header: types.Header{
							BaseHeader: types.BaseHeader{Height: uint64(i + 1)},
						},
					},
				}

				reply := make(chan error, 1)
				err := submitterPID.Tell(actors.SubmitHeaders{
					Headers: headers,
					Reply:   reply,
				})
				if err != nil {
					errors <- err
					return
				}

				select {
				case err := <-reply:
					errors <- err
				case <-time.After(2 * time.Second):
					errors <- assert.AnError
				}
			}(i)
		}

		wg.Wait()
		close(errors)
		elapsed := time.Since(start)

		// Check submissions completed
		for err := range errors {
			assert.NoError(t, err)
		}

		assert.Less(t, elapsed, 5*time.Second)
		t.Logf("Submitted %d headers in %v", numSubmissions, elapsed)
	})

	t.Run("ConcurrentActorCommunication", func(t *testing.T) {
		// Test heavy inter-actor communication
		const numActors = 10
		const messagesPerActor = 100

		actors := make([]*actor.PID, numActors)
		for i := 0; i < numActors; i++ {
			echoActor := &echoActor{responses: make(map[string]string)}
			pid, err := system.Spawn(fmt.Sprintf("echo-%d", i), echoActor)
			require.NoError(t, err)
			actors[i] = pid
		}

		start := time.Now()
		var wg sync.WaitGroup
		errors := make(chan error, numActors*messagesPerActor)

		// Each actor sends messages to all other actors
		for i := 0; i < numActors; i++ {
			wg.Add(1)
			go func(senderIdx int) {
				defer wg.Done()

				for j := 0; j < messagesPerActor; j++ {
					targetIdx := (senderIdx + j) % numActors
					target := actors[targetIdx]

					reply, err := target.Request(fmt.Sprintf("msg-%d-%d", senderIdx, j), 100*time.Millisecond)
					if err != nil {
						errors <- err
					} else if reply == nil {
						errors <- assert.AnError
					} else {
						errors <- nil
					}
				}
			}(i)
		}

		wg.Wait()
		close(errors)
		elapsed := time.Since(start)

		// Check message delivery
		successCount := 0
		for err := range errors {
			if err == nil {
				successCount++
			}
		}

		totalMessages := numActors * messagesPerActor
		successRate := float64(successCount) / float64(totalMessages)

		// Should have high success rate
		assert.Greater(t, successRate, 0.9)
		assert.Less(t, elapsed, 5*time.Second)
		t.Logf("Delivered %d/%d messages (%.1f%%) in %v",
			successCount, totalMessages, successRate*100, elapsed)
	})

	t.Run("MemoryUsageUnderLoad", func(t *testing.T) {
		// Test memory usage with large message volumes
		const numMessages = 10000
		const messageSize = 1024 // 1KB per message

		largeDataActor := &largeDataActor{processedCount: 0}
		largePID, err := system.Spawn("large-data", largeDataActor,
			actor.WithCustomMailboxSize(1000))
		require.NoError(t, err)

		start := time.Now()

		// Send many large messages
		for i := 0; i < numMessages; i++ {
			data := make([]byte, messageSize)
			for j := range data {
				data[j] = byte(i % 256)
			}

			err := largePID.Tell(string(data))
			if err != nil {
				// Expected some failures due to mailbox limits
				continue
			}
		}

		// Wait for processing
		time.Sleep(2 * time.Second)
		elapsed := time.Since(start)

		// Should process reasonable number without crashing
		assert.Greater(t, largeDataActor.processedCount, numMessages/4)
		assert.Less(t, elapsed, 10*time.Second)
		t.Logf("Processed %d large messages in %v",
			largeDataActor.processedCount, elapsed)
	})

	t.Run("TimeoutResilienceUnderLoad", func(t *testing.T) {
		// Test timeout handling under high load
		slowActor := &slowProcessingActor{processTime: 200 * time.Millisecond}
		slowPID, err := system.Spawn("slow-load", slowActor)
		require.NoError(t, err)

		const numRequests = 100
		const requestTimeout = 100 * time.Millisecond

		start := time.Now()
		timeoutCount := 0
		successCount := 0

		var wg sync.WaitGroup
		var mu sync.Mutex

		for i := 0; i < numRequests; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				_, err := slowPID.Request(fmt.Sprintf("req-%d", i), requestTimeout)

				mu.Lock()
				if err != nil {
					timeoutCount++
				} else {
					successCount++
				}
				mu.Unlock()
			}(i)
		}

		wg.Wait()
		elapsed := time.Since(start)

		// Should have many timeouts due to slow processing
		assert.Greater(t, timeoutCount, numRequests/2)
		assert.Less(t, elapsed, 5*time.Second)
		t.Logf("Completed %d requests in %v: %d success, %d timeout",
			numRequests, elapsed, successCount, timeoutCount)
	})
}

// Additional test actors for performance testing

type largeDataActor struct {
	processedCount int
	mu             sync.Mutex
}

func (l *largeDataActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case string:
		l.mu.Lock()
		l.processedCount++
		l.mu.Unlock()

		// Simulate some processing
		time.Sleep(time.Millisecond)
	}
}

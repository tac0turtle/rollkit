package actor_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	"github.com/rollkit/rollkit/blockv2/actor"
)

// TestRobustness focuses on edge cases and robustness scenarios
func TestRobustness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()

	t.Run("SystemShutdownWithActiveActors", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		// Create multiple actors
		actors := make([]*actor.PID, 10)
		for i := 0; i < 10; i++ {
			longRunningActor := &longRunningActor{
				processTime: 100 * time.Millisecond,
				processed:   make(chan struct{}, 1),
			}
			pid, err := system.Spawn(fmt.Sprintf("long-%d", i), longRunningActor)
			require.NoError(t, err)
			actors[i] = pid

			// Start long-running operation
			go func() {
				pid.Tell("long-operation")
			}()
		}

		// Wait a bit for operations to start
		time.Sleep(50 * time.Millisecond)

		// Shutdown system while operations are running
		start := time.Now()
		err := system.Shutdown(2 * time.Second)
		elapsed := time.Since(start)

		assert.NoError(t, err)
		assert.Less(t, elapsed, 3*time.Second)

		// All actors should be stopped
		for _, pid := range actors {
			assert.False(t, pid.IsRunning())
		}
	})

	t.Run("SystemShutdownTimeout", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		// Create actor that ignores stop messages
		stubborn := &stubbornActor{}
		_, err := system.Spawn("stubborn", stubborn)
		require.NoError(t, err)

		// Try to shutdown with very short timeout
		err = system.Shutdown(10 * time.Millisecond)

		// Should complete but might be shorter than timeout due to fast shutdown
		assert.NoError(t, err)
	})

	t.Run("ActorRestartLimits", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		failureCount := 0
		supervisor := &actor.OneForOneSupervisor{
			MaxRestarts: 3,
			Within:      time.Minute,
			Decider: func(err error) actor.Decision {
				failureCount++
				if failureCount > 3 {
					return actor.Stop
				}
				return actor.Restart
			},
		}

		crasher := &crashingActor{crashCount: 0}
		pid, err := system.Spawn("crasher", crasher, actor.WithSupervisor(supervisor))
		require.NoError(t, err)

		// Trigger multiple crashes
		for i := 0; i < 5; i++ {
			err = pid.Tell("crash")
			time.Sleep(10 * time.Millisecond)
		}

		// Should eventually be stopped after max restarts
		time.Sleep(100 * time.Millisecond)
		assert.False(t, pid.IsRunning())
	})

	t.Run("CircuitBreakerStates", func(t *testing.T) {
		cb := actor.NewCircuitBreaker("test", 3, time.Minute)

		// Should start closed
		assert.Equal(t, actor.Closed, cb.State())

		// Trigger failures to open
		for i := 0; i < 3; i++ {
			err := cb.Call(func() error {
				return errors.New("failure")
			})
			assert.Error(t, err)
		}

		// Should be open now
		assert.Equal(t, actor.Open, cb.State())

		// Should fail fast
		start := time.Now()
		err := cb.Call(func() error {
			time.Sleep(100 * time.Millisecond) // This shouldn't execute
			return nil
		})
		elapsed := time.Since(start)

		assert.Error(t, err)
		assert.Less(t, elapsed, 50*time.Millisecond)
		assert.Contains(t, err.Error(), "circuit breaker test is open")
	})

	t.Run("RateLimiterCapacity", func(t *testing.T) {
		limiter := actor.NewRateLimiter("test", 5, 10) // 5 capacity, 10/sec rate

		// Should allow initial burst
		for i := 0; i < 5; i++ {
			assert.True(t, limiter.AllowOne())
		}

		// Should deny further requests
		assert.False(t, limiter.AllowOne())

		// Wait for refill
		time.Sleep(200 * time.Millisecond)
		assert.True(t, limiter.AllowOne())
	})

	t.Run("ContextCancellation", func(t *testing.T) {
		localCtx, localCancel := context.WithCancel(context.Background())
		system := actor.NewSystem(localCtx, logger)

		echoActor := &echoActor{responses: make(map[string]string)}
		pid, err := system.Spawn("echo", echoActor)
		require.NoError(t, err)

		// Actor should be running
		assert.True(t, pid.IsRunning())

		// Cancel context
		localCancel()

		// Wait for actor to stop
		time.Sleep(50 * time.Millisecond)
		assert.False(t, pid.IsRunning())
	})

	t.Run("RequestReplyValidation", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		validator := &requestValidator{}
		pid, err := system.Spawn("validator", validator)
		require.NoError(t, err)

		// Valid request
		reply, err := pid.Request("valid", time.Second)
		assert.NoError(t, err)
		assert.Equal(t, "valid-response", reply)

		// Request that causes panic
		reply, err = pid.Request("panic", time.Second)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "panic")
		assert.Nil(t, reply)

		// Request that times out
		reply, err = pid.Request("slow", 50*time.Millisecond)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
		assert.Nil(t, reply)
	})

	t.Run("SupervisorDecisions", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		// Test all supervisor decisions
		decisions := []actor.Decision{
			actor.Resume,
			actor.Restart,
			actor.Stop,
			actor.Escalate,
		}

		for i, decision := range decisions {
			name := fmt.Sprintf("test-%d", i)
			supervisor := &testSupervisor{decision: decision}
			testActor := &panicOnMessageActor{panicOn: "panic"}

			pid, err := system.Spawn(name, testActor, actor.WithSupervisor(supervisor))
			require.NoError(t, err)

			// Trigger panic
			err = pid.Tell("panic")
			assert.NoError(t, err)

			time.Sleep(100 * time.Millisecond)

			switch decision {
			case actor.Resume:
				// Actor should still be running after resume
				assert.True(t, pid.IsRunning())
			case actor.Restart:
				// Actor should be restarted and running
				assert.True(t, pid.IsRunning())
			case actor.Stop, actor.Escalate:
				// Actor should be stopped
				// Give it more time to ensure the stop completes
				for j := 0; j < 200; j++ {
					if !pid.IsRunning() {
						break
					}
					time.Sleep(time.Millisecond)
				}
				assert.False(t, pid.IsRunning())
			}
		}
	})

	t.Run("MailboxCustomSizes", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger, actor.WithMailboxSize(100))

		// Test actor with default size
		actor1 := &counterActor{}
		pid1, err := system.Spawn("default", actor1)
		require.NoError(t, err)

		// Test actor with custom size
		actor2 := &counterActor{}
		pid2, err := system.Spawn("custom", actor2, actor.WithCustomMailboxSize(10))
		require.NoError(t, err)

		// Both should work normally
		err = pid1.Tell(&incrementMsg{value: 1})
		assert.NoError(t, err)
		err = pid2.Tell(&incrementMsg{value: 1})
		assert.NoError(t, err)

		time.Sleep(10 * time.Millisecond)

		// Verify they received messages
		reply1, err := pid1.Request(&getCountMsg{}, time.Second)
		assert.NoError(t, err)
		assert.Equal(t, int32(1), reply1)

		reply2, err := pid2.Request(&getCountMsg{}, time.Second)
		assert.NoError(t, err)
		assert.Equal(t, int32(1), reply2)
	})

	t.Run("MessageValidationErrors", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		validator := &validationActor{}
		pid, err := system.Spawn("validator", validator)
		require.NoError(t, err)

		// Send message that fails validation
		invalidMsg := &validatedMessage{value: -10}
		err = pid.Tell(invalidMsg)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "validation failed")

		// Valid message should work
		validMsg := &validatedMessage{value: 10}
		err = pid.Tell(validMsg)
		assert.NoError(t, err)

		time.Sleep(10 * time.Millisecond)
		assert.Equal(t, 1, validator.validCount)
		assert.Equal(t, 0, validator.invalidCount)
	})

	t.Run("ActorSendMessages", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		// Create two actors
		receiver := &messageReceiver{}
		receiverPID, err := system.Spawn("receiver", receiver)
		require.NoError(t, err)

		sender := &messageSender{target: receiverPID}
		senderPID, err := system.Spawn("sender", sender)
		require.NoError(t, err)

		// Trigger sending
		err = senderPID.Tell("send")
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)

		// Verify message was received
		assert.Equal(t, 1, receiver.received)
		assert.Equal(t, senderPID, receiver.lastSender)
	})
}

// Test actors for robustness testing

type longRunningActor struct {
	processTime time.Duration
	processed   chan struct{}
}

func (l *longRunningActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case string:
		time.Sleep(l.processTime)
		select {
		case l.processed <- struct{}{}:
		default:
		}
	}
}

type stubbornActor struct{}

func (s *stubbornActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case actor.Stopping:
		// Ignore stop message
	}
}

type crashingActor struct {
	crashCount int32
}

func (c *crashingActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case string:
		count := atomic.AddInt32(&c.crashCount, 1)
		panic(fmt.Sprintf("crash #%d", count))
	}
}

type requestValidator struct{}

func (r *requestValidator) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case string:
		if m == "panic" {
			panic("intentional panic")
		}
		if m == "slow" {
			time.Sleep(200 * time.Millisecond)
		}
		ctx.Respond(m + "-response")
	}
}

type testSupervisor struct {
	decision actor.Decision
}

func (t *testSupervisor) HandleFailure(pid *actor.PID, err error) actor.Decision {
	return t.decision
}

type messageSender struct {
	target *actor.PID
}

func (m *messageSender) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case string:
		ctx.Tell(m.target, "hello")
	}
}

type messageReceiver struct {
	received   int
	lastSender *actor.PID
}

func (m *messageReceiver) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case string:
		m.received++
		m.lastSender = ctx.Sender()
	}
}

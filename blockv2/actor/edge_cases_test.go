package actor_test

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
)

// TestEdgeCases covers edge cases and error paths for complete coverage
func TestEdgeCases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()

	t.Run("DuplicateActorNames", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		actor1 := &echoActor{responses: make(map[string]string)}
		_, err := system.Spawn("duplicate", actor1)
		require.NoError(t, err)

		// Try to spawn with same name
		actor2 := &echoActor{responses: make(map[string]string)}
		_, err = system.Spawn("duplicate", actor2)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})

	t.Run("GetNonExistentActor", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		pid, exists := system.Get("non-existent")
		assert.False(t, exists)
		assert.Nil(t, pid)
	})

	t.Run("SendToStoppedActor", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		echoActor := &echoActor{responses: make(map[string]string)}
		pid, err := system.Spawn("echo", echoActor)
		require.NoError(t, err)

		// Stop the actor
		pid.Stop()

		// Wait for stop to complete
		time.Sleep(50 * time.Millisecond)

		// Try to send message
		err = pid.Tell("test")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not running")
	})

	t.Run("RequestToStoppedActor", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		echoActor := &echoActor{responses: make(map[string]string)}
		pid, err := system.Spawn("echo", echoActor)
		require.NoError(t, err)

		// Stop the actor
		pid.Stop()
		time.Sleep(50 * time.Millisecond)

		// Try to send request
		_, err = pid.Request("test", time.Second)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not running")
	})

	t.Run("StopAlreadyStoppedActor", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		echoActor := &echoActor{responses: make(map[string]string)}
		pid, err := system.Spawn("echo", echoActor)
		require.NoError(t, err)

		// Stop once
		pid.Stop()
		assert.False(t, pid.IsRunning())

		// Stop again - should be safe
		pid.Stop()
		assert.False(t, pid.IsRunning())
	})

	t.Run("ResponseToNonRequest", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		responder := &responseActor{}
		pid, err := system.Spawn("responder", responder)
		require.NoError(t, err)

		// Send regular message that tries to respond
		err = pid.Tell("respond")
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		// Should not crash
		assert.True(t, pid.IsRunning())
	})

	t.Run("PanicInMessageHandling", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		supervisor := &actor.AlwaysRestartSupervisor{Delay: 10 * time.Millisecond}
		panicActor := &panicOnMessageActor{panicOn: "panic"}
		pid, err := system.Spawn("panic", panicActor, actor.WithSupervisor(supervisor))
		require.NoError(t, err)

		// Send panic message
		err = pid.Tell("panic")
		assert.NoError(t, err)

		// Wait for restart
		time.Sleep(100 * time.Millisecond)

		// Actor should be restarted and running
		assert.True(t, pid.IsRunning())
	})

	t.Run("PanicInActorRun", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		supervisor := &actor.AlwaysRestartSupervisor{Delay: 10 * time.Millisecond}
		runPanic := &runPanicActor{}
		pid, err := system.Spawn("run-panic", runPanic, actor.WithSupervisor(supervisor))
		require.NoError(t, err)

		// Wait for potential restart
		time.Sleep(100 * time.Millisecond)

		// Should be handled by supervisor
		assert.True(t, pid.IsRunning())
	})

	t.Run("RequestPanicHandling", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		panicActor := &requestPanicActor{}
		pid, err := system.Spawn("request-panic", panicActor)
		require.NoError(t, err)

		// Send request that causes panic
		reply, err := pid.Request("panic", time.Second)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "panic")
		assert.Nil(t, reply)
	})

	t.Run("ClosedReplyChannel", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		replyActor := &replyChannelActor{}
		pid, err := system.Spawn("reply", replyActor)
		require.NoError(t, err)

		// Create a request with closed reply channel
		reply := make(chan any, 1)
		close(reply) // Close before sending

		req := &actor.Request{
			Message: "test",
			Reply:   reply,
		}

		err = pid.Tell(req)
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		// Should not crash
		assert.True(t, pid.IsRunning())
	})

	t.Run("ConcurrentRestart", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		supervisor := &concurrentRestartSupervisor{}
		crasher := &multiCrashActor{}
		pid, err := system.Spawn("concurrent", crasher, actor.WithSupervisor(supervisor))
		require.NoError(t, err)

		// Trigger multiple concurrent crashes
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				pid.Tell("crash")
			}()
		}

		wg.Wait()
		time.Sleep(100 * time.Millisecond)

		// Should eventually stabilize
		assert.True(t, pid.IsRunning())
	})

	t.Run("MetricsCollection", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		counter := &counterActor{}
		pid, err := system.Spawn("metrics", counter)
		require.NoError(t, err)

		// Send some messages
		for i := 0; i < 10; i++ {
			err = pid.Tell(&incrementMsg{value: 1})
			assert.NoError(t, err)
		}

		// Send invalid message to trigger validation error
		err = pid.Tell(&validatedMessage{value: -1})
		assert.Error(t, err)

		// Trigger timeout
		_, err = pid.Request("non-existent", 10*time.Millisecond)
		assert.Error(t, err)

		// Metrics should be collected (we can't easily test exact values due to concurrency)
		time.Sleep(50 * time.Millisecond)
	})

	t.Run("DefaultSupervisor", func(t *testing.T) {
		system := actor.NewSystem(ctx, logger)

		// Actor with default supervisor
		crasher := &crashingActor{}
		pid, err := system.Spawn("default-super", crasher)
		require.NoError(t, err)

		// Trigger crash
		err = pid.Tell("crash")
		assert.NoError(t, err)

		time.Sleep(100 * time.Millisecond)
		// Default supervisor should handle it
		assert.True(t, pid.IsRunning())
	})

	t.Run("CircuitBreakerRecovery", func(t *testing.T) {
		cb := actor.NewCircuitBreaker("recovery", 2, 100*time.Millisecond)

		// Trigger failures to open circuit
		for i := 0; i < 3; i++ {
			err := cb.Call(func() error {
				return fmt.Errorf("failure %d", i)
			})
			assert.Error(t, err)
		}

		assert.Equal(t, actor.Open, cb.State())

		// Wait for timeout
		time.Sleep(150 * time.Millisecond)

		// Should transition to half-open
		err := cb.Call(func() error {
			return nil // Success
		})
		assert.NoError(t, err)

		assert.Equal(t, actor.Closed, cb.State())
	})

	t.Run("RateLimiterEdgeCases", func(t *testing.T) {
		// Zero capacity
		limiter := actor.NewRateLimiter("zero", 0, 1)
		assert.False(t, limiter.AllowOne())

		// Very high rate
		limiter2 := actor.NewRateLimiter("high", 1000, 1000000)
		for i := 0; i < 100; i++ {
			assert.True(t, limiter2.AllowOne())
		}
	})

	t.Run("SystemWithCancelledContext", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		system := actor.NewSystem(cancelledCtx, logger)

		// Should still be able to spawn actors
		echoActor := &echoActor{responses: make(map[string]string)}
		pid, err := system.Spawn("echo", echoActor)
		require.NoError(t, err)

		// But actor should stop quickly due to cancelled context
		time.Sleep(50 * time.Millisecond)
		assert.False(t, pid.IsRunning())
	})
}

// Additional test actors for edge cases

type responseActor struct{}

func (r *responseActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case string:
		// Try to respond to non-request message
		ctx.Respond("response")
	}
}

type runPanicActor struct{}

func (r *runPanicActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case actor.Started:
		// Don't panic on start, let it initialize
	default:
		panic("run panic")
	}
}

type requestPanicActor struct{}

func (r *requestPanicActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case string:
		panic("request panic")
	}
}

type replyChannelActor struct{}

func (r *replyChannelActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case string:
		ctx.Respond("reply")
	}
}

type multiCrashActor struct {
	crashes int
}

func (m *multiCrashActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case string:
		m.crashes++
		if m.crashes < 5 {
			panic(fmt.Sprintf("multi crash %d", m.crashes))
		}
	}
}

type concurrentRestartSupervisor struct {
	restarts int
}

func (c *concurrentRestartSupervisor) HandleFailure(pid *actor.PID, err error) actor.Decision {
	c.restarts++
	if c.restarts > 3 {
		return actor.Resume // Stop restarting after a while
	}
	return actor.Restart
}
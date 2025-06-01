package blockv2_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	
	"cosmossdk.io/log"
	"github.com/rollkit/rollkit/blockv2/actor"
)

// TestActorSystem tests basic actor functionality
func TestActorSystem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)
	
	// Create a simple echo actor
	echoActor := &EchoActor{
		responses: make(map[string]string),
	}
	
	pid, err := system.Spawn("echo", echoActor)
	require.NoError(t, err)
	require.NotNil(t, pid)
	
	// Test tell
	err = pid.Tell("hello")
	assert.NoError(t, err)
	
	// Test request-reply
	reply, err := pid.Request("ping", time.Second)
	assert.NoError(t, err)
	assert.Equal(t, "pong", reply)
	
	// Test validation
	err = pid.Tell(InvalidMessage{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "validation failed")
	
	// Shutdown
	err = system.Shutdown(time.Second)
	assert.NoError(t, err)
}

// TestCircuitBreaker tests the circuit breaker functionality
func TestCircuitBreaker(t *testing.T) {
	cb := actor.NewCircuitBreaker("test", 3, time.Second)
	
	// Success calls
	for i := 0; i < 3; i++ {
		err := cb.Call(func() error { return nil })
		assert.NoError(t, err)
	}
	assert.Equal(t, actor.Closed, cb.State())
	
	// Failures open the circuit
	for i := 0; i < 3; i++ {
		err := cb.Call(func() error { return assert.AnError })
		assert.Error(t, err)
	}
	assert.Equal(t, actor.Open, cb.State())
	
	// Circuit is open, calls fail immediately
	err := cb.Call(func() error { return nil })
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker test is open")
	
	// Wait for timeout
	time.Sleep(time.Second + 100*time.Millisecond)
	
	// Circuit should be half-open, allowing limited calls
	err = cb.Call(func() error { return nil })
	assert.NoError(t, err)
}

// TestRateLimiter tests the rate limiter functionality
func TestRateLimiter(t *testing.T) {
	rl := actor.NewRateLimiter("test", 5, 5) // 5 tokens, 5 per second
	
	// Use all tokens
	for i := 0; i < 5; i++ {
		assert.True(t, rl.AllowOne())
	}
	
	// No more tokens
	assert.False(t, rl.AllowOne())
	
	// Wait for refill
	time.Sleep(time.Second)
	
	// Should have tokens again
	assert.True(t, rl.AllowOne())
}

// TestSupervisor tests actor supervision
func TestSupervisor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)
	
	// Create actor that panics
	panicActor := &PanicActor{
		panicOn: 3,
		count:   0,
	}
	
	// Spawn with restart supervisor
	pid, err := system.Spawn("panic", panicActor,
		actor.WithSupervisor(&actor.AlwaysRestartSupervisor{
			Delay: 100 * time.Millisecond,
		}))
	require.NoError(t, err)
	
	// Send messages that will cause panic
	for i := 0; i < 5; i++ {
		err = pid.Tell("message")
		assert.NoError(t, err)
		time.Sleep(200 * time.Millisecond)
	}
	
	// Actor should still be running after restarts
	assert.True(t, pid.IsRunning())
	
	system.Shutdown(time.Second)
}

// Test actors

type EchoActor struct {
	responses map[string]string
}

func (e *EchoActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case string:
		if m == "ping" {
			ctx.Respond("pong")
		}
		ctx.Logger().Debug("received string", "msg", m)
	case InvalidMessage:
		// Should not reach here due to validation
		panic("should not receive invalid message")
	}
}

type InvalidMessage struct{}

func (InvalidMessage) Validate() error {
	return assert.AnError
}

type PanicActor struct {
	panicOn int
	count   int
}

func (p *PanicActor) Receive(ctx actor.Context, msg any) {
	p.count++
	if p.count == p.panicOn {
		panic("intentional panic")
	}
	ctx.Logger().Debug("processed message", "count", p.count)
}
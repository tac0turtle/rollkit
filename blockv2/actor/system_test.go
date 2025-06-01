package actor_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	"github.com/rollkit/rollkit/blockv2/actor"
)

// TestActorSystem tests the basic functionality of the actor system
func TestActorSystem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Test actor spawning
	echoActor := &echoActor{responses: make(map[string]string)}
	pid, err := system.Spawn("echo", echoActor)
	require.NoError(t, err)
	require.NotNil(t, pid)
	assert.Equal(t, "echo", pid.Name())
	assert.True(t, pid.IsRunning())

	// Test duplicate actor name
	_, err = system.Spawn("echo", echoActor)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	// Test actor retrieval
	retrievedPID, ok := system.Get("echo")
	assert.True(t, ok)
	assert.Equal(t, pid, retrievedPID)

	// Test non-existent actor
	_, ok = system.Get("non-existent")
	assert.False(t, ok)

	// Test shutdown
	err = system.Shutdown(time.Second)
	assert.NoError(t, err)
	assert.False(t, pid.IsRunning())
}

// TestActorMessaging tests message passing between actors
func TestActorMessaging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create counter actor
	counter := &counterActor{}
	counterPID, err := system.Spawn("counter", counter)
	require.NoError(t, err)

	// Test Tell (fire-and-forget)
	err = counterPID.Tell(&incrementMsg{value: 5})
	assert.NoError(t, err)

	// Give time for message processing
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, int32(5), counter.count.Load())

	// Test multiple messages
	for i := 0; i < 10; i++ {
		err = counterPID.Tell(&incrementMsg{value: 1})
		assert.NoError(t, err)
	}

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(15), counter.count.Load())

	// Test Request-Reply pattern
	reply, err := counterPID.Request(&getCountMsg{}, time.Second)
	assert.NoError(t, err)
	assert.Equal(t, int32(15), reply.(int32))
}

// TestActorPanic tests panic recovery
func TestActorPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create panic actor with restart supervisor
	panicActor := &panicOnMessageActor{
		panicOn: "panic",
	}

	pid, err := system.Spawn("panic", panicActor,
		actor.WithSupervisor(&actor.AlwaysRestartSupervisor{
			Delay: 10 * time.Millisecond,
		}))
	require.NoError(t, err)

	// Send normal message
	err = pid.Tell("normal")
	assert.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, int32(1), panicActor.processed.Load())

	// Send panic message
	err = pid.Tell("panic")
	assert.NoError(t, err)
	time.Sleep(50 * time.Millisecond)

	// Actor should have restarted
	assert.True(t, pid.IsRunning())

	// Send another normal message to verify restart
	err = pid.Tell("normal")
	assert.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, int32(2), panicActor.processed.Load())
}

// TestMailboxOverflow tests behavior when mailbox is full
func TestMailboxOverflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger, actor.WithMailboxSize(10))

	// Create slow actor
	slowActor := &slowProcessingActor{
		processTime: 100 * time.Millisecond,
	}
	pid, err := system.Spawn("slow", slowActor, actor.WithCustomMailboxSize(5))
	require.NoError(t, err)

	// Send more messages than mailbox can hold
	var sendErrors int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := pid.Tell(n); err != nil {
				atomic.AddInt32(&sendErrors, 1)
			}
		}(i)
	}

	wg.Wait()

	// Should have some send errors due to full mailbox
	assert.Greater(t, atomic.LoadInt32(&sendErrors), int32(0))
}

// TestMessageValidation tests message validation
func TestMessageValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create actor
	actor := &validationActor{}
	pid, err := system.Spawn("validator", actor)
	require.NoError(t, err)

	// Send valid message
	err = pid.Tell(&validatedMessage{value: 10})
	assert.NoError(t, err)

	// Send invalid message
	err = pid.Tell(&validatedMessage{value: -1})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "validation failed")

	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 1, actor.validCount)
	assert.Equal(t, 0, actor.invalidCount)
}

// TestActorStopBehavior tests graceful stopping
func TestActorStopBehavior(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create actor that tracks stopping
	stoppableActor := newLifecycleActor()
	pid, err := system.Spawn("stoppable", stoppableActor)
	require.NoError(t, err)

	// Wait for started message to be processed
	select {
	case <-stoppableActor.startedCh:
		assert.True(t, stoppableActor.started)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for actor to start")
	}

	// Stop actor
	pid.Stop()
	
	// Wait for stopped message to be processed
	select {
	case <-stoppableActor.stoppedCh:
		assert.True(t, stoppableActor.stopped)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for actor to stop")
	}

	assert.False(t, pid.IsRunning())

	// Sending to stopped actor should fail
	err = pid.Tell("message")
	assert.Error(t, err)
}

// TestSupervisorStrategies tests different supervisor strategies
func TestSupervisorStrategies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	t.Run("OneForOneSupervisor", func(t *testing.T) {
		failureCount := 0
		supervisor := &actor.OneForOneSupervisor{
			MaxRestarts: 3,
			Within:      time.Minute,
			Decider: func(err error) actor.Decision {
				failureCount++
				if failureCount >= 3 {
					return actor.Stop
				}
				return actor.Restart
			},
		}

		panicActor := &panicOnMessageActor{panicOn: "panic"}
		pid, err := system.Spawn("supervised", panicActor,
			actor.WithSupervisor(supervisor))
		require.NoError(t, err)

		// Trigger panics
		for i := 0; i < 3; i++ {
			err = pid.Tell("panic")
			assert.NoError(t, err)
			time.Sleep(50 * time.Millisecond)
		}

		// After 3 failures, actor should be stopped
		assert.False(t, pid.IsRunning())
	})
}

// TestRequestTimeout tests request timeout behavior
func TestRequestTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create actor that doesn't respond
	nonResponsiveActor := &nonResponsiveActor{}
	pid, err := system.Spawn("non-responsive", nonResponsiveActor)
	require.NoError(t, err)

	// Request with timeout
	_, err = pid.Request("request", 100*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

// Test actors

type echoActor struct {
	responses map[string]string
	mu        sync.Mutex
}

func (e *echoActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case string:
		e.mu.Lock()
		e.responses[m] = m + "-echo"
		e.mu.Unlock()
	}
}

type counterActor struct {
	count atomic.Int32
}

type incrementMsg struct {
	value int32
}

type getCountMsg struct{}

func (c *counterActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case *incrementMsg:
		c.count.Add(m.value)
	case *getCountMsg:
		ctx.Respond(c.count.Load())
	}
}

type panicOnMessageActor struct {
	panicOn   string
	processed atomic.Int32
}

func (p *panicOnMessageActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case string:
		if m == p.panicOn {
			panic("intentional panic")
		}
		p.processed.Add(1)
	}
}

type slowProcessingActor struct {
	processTime time.Duration
}

func (s *slowProcessingActor) Receive(ctx actor.Context, msg any) {
	time.Sleep(s.processTime)
}

type validatedMessage struct {
	value int
}

func (v *validatedMessage) Validate() error {
	if v.value < 0 {
		return fmt.Errorf("value must be positive")
	}
	return nil
}

type validationActor struct {
	validCount   int
	invalidCount int
}

func (v *validationActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case *validatedMessage:
		v.validCount++
	case actor.Started, actor.Stopping:
		// Ignore system messages
	default:
		v.invalidCount++
	}
}

type lifecycleActor struct {
	started   bool
	stopped   bool
	mu        sync.Mutex
	startedCh chan struct{}
	stoppedCh chan struct{}
}

func newLifecycleActor() *lifecycleActor {
	return &lifecycleActor{
		startedCh: make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

func (l *lifecycleActor) Receive(ctx actor.Context, msg any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	
	switch msg.(type) {
	case actor.Started:
		if !l.started {
			l.started = true
			close(l.startedCh)
		}
	case actor.Stopping:
		if !l.stopped {
			l.stopped = true
			close(l.stoppedCh)
		}
	}
}

type nonResponsiveActor struct{}

func (n *nonResponsiveActor) Receive(ctx actor.Context, msg any) {
	// Don't respond to any messages
}
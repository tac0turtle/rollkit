package actor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cosmossdk.io/log"
)

// System is the root of the actor system
type System struct {
	ctx      context.Context
	cancel   context.CancelFunc
	logger   log.Logger
	actors   map[string]*PID
	mu       sync.RWMutex
	mailboxSize int
}

// NewSystem creates a new actor system
func NewSystem(ctx context.Context, logger log.Logger, opts ...SystemOption) *System {
	ctx, cancel := context.WithCancel(ctx)
	s := &System{
		ctx:      ctx,
		cancel:   cancel,
		logger:   logger,
		actors:   make(map[string]*PID),
		mailboxSize: 1000, // default
	}
	
	for _, opt := range opts {
		opt(s)
	}
	
	return s
}

// SystemOption configures the actor system
type SystemOption func(*System)

// WithMailboxSize sets the default mailbox size for actors
func WithMailboxSize(size int) SystemOption {
	return func(s *System) {
		s.mailboxSize = size
	}
}

// Spawn creates a new actor and returns its PID
func (s *System) Spawn(name string, actor Actor, opts ...SpawnOption) (*PID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	if _, exists := s.actors[name]; exists {
		return nil, fmt.Errorf("actor %s already exists", name)
	}
	
	config := &spawnConfig{
		mailboxSize: s.mailboxSize,
		supervisor:  DefaultSupervisor(),
	}
	
	for _, opt := range opts {
		opt(config)
	}
	
	ctx, cancel := context.WithCancel(s.ctx)
	
	pid := &PID{
		name:     name,
		system:   s,
		actor:    actor,
		mailbox:  make(chan Envelope, config.mailboxSize),
		ctx:      ctx,
		cancel:   cancel,
		logger:   s.logger.With("actor", name),
		supervisor: config.supervisor,
		metrics:  NewMetrics(name),
		started:  make(chan struct{}),
	}
	
	s.actors[name] = pid
	
	// Start the actor
	go pid.run()
	
	// Wait for actor to be ready
	<-pid.started
	
	// Send Started message
	pid.Tell(Started{})
	
	return pid, nil
}

// Get returns an actor by name
func (s *System) Get(name string) (*PID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.actors[name], s.actors[name] != nil
}

// Shutdown gracefully stops all actors
func (s *System) Shutdown(timeout time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	// Send stop messages to all actors
	var wg sync.WaitGroup
	for _, pid := range s.actors {
		wg.Add(1)
		go func(p *PID) {
			defer wg.Done()
			p.Stop()
		}(pid)
	}
	
	// Wait for actors to stop or timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	
	select {
	case <-done:
		s.cancel()
		return nil
	case <-time.After(timeout):
		s.cancel()
		return fmt.Errorf("shutdown timed out after %v", timeout)
	}
}

type spawnConfig struct {
	mailboxSize int
	supervisor  SupervisorStrategy
}

// SpawnOption configures actor spawning
type SpawnOption func(*spawnConfig)

// WithCustomMailboxSize sets a custom mailbox size for this actor
func WithCustomMailboxSize(size int) SpawnOption {
	return func(c *spawnConfig) {
		c.mailboxSize = size
	}
}

// WithSupervisor sets a custom supervisor strategy
func WithSupervisor(supervisor SupervisorStrategy) SpawnOption {
	return func(c *spawnConfig) {
		c.supervisor = supervisor
	}
}
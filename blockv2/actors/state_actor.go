package actors

import (
	"context"
	"fmt"
	"sync"

	"cosmossdk.io/log"

	"github.com/rollkit/rollkit/blockv2/actor"
	"github.com/rollkit/rollkit/pkg/store"
	"github.com/rollkit/rollkit/types"
)

// StateActor manages blockchain state as the single source of truth
type StateActor struct {
	mu          sync.RWMutex
	state       types.State
	store       store.Store
	subscribers map[string]chan<- StateAdvanced
}

// NewStateActor creates a new state actor
func NewStateActor(store store.Store, initialState types.State) *StateActor {
	return &StateActor{
		state:       initialState,
		store:       store,
		subscribers: make(map[string]chan<- StateAdvanced),
	}
}

// Receive processes incoming messages
func (s *StateActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case actor.Started:
		ctx.Logger().Info("state actor started", "height", s.state.LastBlockHeight)

	case GetState:
		s.mu.RLock()
		stateCopy := s.state
		s.mu.RUnlock()

		select {
		case m.Reply <- stateCopy:
		default:
			ctx.Logger().Error("failed to send state reply")
		}

	case UpdateState:
		err := s.updateState(ctx.Logger(), m.State)

		select {
		case m.Reply <- err:
		default:
			ctx.Logger().Error("failed to send update reply")
		}

		if err == nil {
			s.notifySubscribers(ctx, m.State)
		}

	case *SubscribeState:
		s.mu.Lock()
		s.subscribers[m.ID] = m.Updates
		s.mu.Unlock()

		ctx.Logger().Debug("state subscriber added", "id", m.ID)

	case *UnsubscribeState:
		s.mu.Lock()
		delete(s.subscribers, m.ID)
		s.mu.Unlock()

		ctx.Logger().Debug("state subscriber removed", "id", m.ID)

	case actor.Stopping:
		s.mu.Lock()
		for _, ch := range s.subscribers {
			close(ch)
		}
		s.subscribers = make(map[string]chan<- StateAdvanced)
		s.mu.Unlock()
	}
}

// updateState atomically updates the state
func (s *StateActor) updateState(logger log.Logger, newState types.State) error {
	// Validate state transition
	s.mu.RLock()
	currentHeight := s.state.LastBlockHeight
	s.mu.RUnlock()

	if newState.LastBlockHeight != currentHeight+1 {
		return fmt.Errorf("invalid height transition: %d -> %d",
			currentHeight, newState.LastBlockHeight)
	}

	// Persist to store first
	storeCtx := context.Background()
	if err := s.store.UpdateState(storeCtx, newState); err != nil {
		return fmt.Errorf("failed to persist state: %w", err)
	}

	// Update in-memory state
	s.mu.Lock()
	s.state = newState
	s.mu.Unlock()

	logger.Info("state updated",
		"height", newState.LastBlockHeight,
		"appHash", fmt.Sprintf("%X", newState.AppHash))

	return nil
}

// notifySubscribers sends state updates to all subscribers
func (s *StateActor) notifySubscribers(ctx actor.Context, state types.State) {
	s.mu.RLock()
	subscribers := make([]chan<- StateAdvanced, 0, len(s.subscribers))
	for _, ch := range s.subscribers {
		subscribers = append(subscribers, ch)
	}
	s.mu.RUnlock()

	event := StateAdvanced{
		Height:   state.LastBlockHeight,
		AppHash:  state.AppHash,
		DAHeight: state.DAHeight,
	}

	for _, ch := range subscribers {
		select {
		case ch <- event:
		default:
			ctx.Logger().Warn("subscriber channel full")
		}
	}
}

// SaveState implements StatefulActor
func (s *StateActor) SaveState() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// In production, use proper serialization
	return s.state.Bytes(), nil
}

// RestoreState implements StatefulActor
func (s *StateActor) RestoreState(data []byte) error {
	// In production, use proper deserialization
	return fmt.Errorf("not implemented")
}

// Message types specific to StateActor
type (
	// SubscribeState subscribes to state updates
	SubscribeState struct {
		ID      string
		Updates chan<- StateAdvanced
	}

	// UnsubscribeState removes a state subscription
	UnsubscribeState struct {
		ID string
	}
)

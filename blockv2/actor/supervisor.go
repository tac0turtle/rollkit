package actor

import (
	"sync"
	"time"
)

// Decision represents what to do with a failed actor
type Decision int

const (
	// Resume continues processing messages
	Resume Decision = iota
	// Restart restarts the actor
	Restart
	// Stop stops the actor
	Stop
	// Escalate escalates to parent supervisor
	Escalate
)

// SupervisorStrategy decides what to do when an actor fails
type SupervisorStrategy interface {
	HandleFailure(actor *PID, err error) Decision
}

// DefaultSupervisor returns the default supervisor strategy
func DefaultSupervisor() SupervisorStrategy {
	return &defaultSupervisor{
		maxRestarts: 3,
		within:      1 * time.Minute,
	}
}

type defaultSupervisor struct {
	maxRestarts int32
	within      time.Duration
}

func (s *defaultSupervisor) HandleFailure(actor *PID, err error) Decision {
	restarts := actor.restarts.Load()
	if restarts >= s.maxRestarts {
		return Stop
	}
	return Restart
}

// AlwaysRestartSupervisor always restarts failed actors
type AlwaysRestartSupervisor struct {
	Delay time.Duration
}

func (s *AlwaysRestartSupervisor) HandleFailure(actor *PID, err error) Decision {
	if s.Delay > 0 {
		time.Sleep(s.Delay)
	}
	return Restart
}

// OneForOneSupervisor implements one-for-one supervision
// (only restart the failed actor)
type OneForOneSupervisor struct {
	MaxRestarts int
	Within      time.Duration
	Decider     func(err error) Decision

	mu           sync.Mutex
	restartTimes map[*PID][]time.Time
}

func (s *OneForOneSupervisor) HandleFailure(actor *PID, err error) Decision {
	if s.Decider != nil {
		decision := s.Decider(err)
		if decision != Restart {
			return decision
		}
	}

	if actor == nil {
		return Escalate
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.restartTimes == nil {
		s.restartTimes = make(map[*PID][]time.Time)
	}

	now := time.Now()
	times := s.restartTimes[actor]

	// Remove old restart times outside the window
	cutoff := now.Add(-s.Within)
	filteredTimes := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			filteredTimes = append(filteredTimes, t)
		}
	}

	// Add current restart time
	filteredTimes = append(filteredTimes, now)
	s.restartTimes[actor] = filteredTimes

	// Check if we've exceeded max restarts in time window
	if len(filteredTimes) > s.MaxRestarts {
		delete(s.restartTimes, actor) // Clean up
		return Escalate
	}

	return Restart
}

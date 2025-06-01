package actor

import (
	"fmt"
	"sync"
	"time"
)

// CircuitState represents the state of a circuit breaker
type CircuitState int

const (
	// Closed allows requests through
	Closed CircuitState = iota
	// Open blocks all requests
	Open
	// HalfOpen allows limited requests for testing
	HalfOpen
)

// String returns the string representation of the circuit state
func (cs CircuitState) String() string {
	switch cs {
	case Closed:
		return "Closed"
	case Open:
		return "Open"
	case HalfOpen:
		return "HalfOpen"
	default:
		return "Unknown"
	}
}

// CircuitBreaker protects actors from cascading failures
type CircuitBreaker struct {
	mu              sync.RWMutex
	name            string
	state           CircuitState
	failures        int
	successes       int
	maxFailures     int
	successThreshold int
	timeout         time.Duration
	lastFailureTime time.Time
	halfOpenLimit   int
	halfOpenCurrent int
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(name string, maxFailures int, timeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		name:             name,
		state:            Closed,
		maxFailures:      maxFailures,
		successThreshold: maxFailures / 2,
		timeout:          timeout,
		halfOpenLimit:    3,
	}
}

// Call executes a function with circuit breaker protection
func (cb *CircuitBreaker) Call(fn func() error) error {
	if !cb.canExecute() {
		return fmt.Errorf("circuit breaker %s is open", cb.name)
	}
	
	err := fn()
	cb.recordResult(err == nil)
	return err
}

// canExecute checks if a request can proceed
func (cb *CircuitBreaker) canExecute() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	
	switch cb.state {
	case Closed:
		return true
		
	case Open:
		if time.Since(cb.lastFailureTime) > cb.timeout {
			cb.state = HalfOpen
			cb.halfOpenCurrent = 0
			return true
		}
		return false
		
	case HalfOpen:
		if cb.halfOpenCurrent < cb.halfOpenLimit {
			cb.halfOpenCurrent++
			return true
		}
		return false
		
	default:
		return false
	}
}

// recordResult records the result of an execution
func (cb *CircuitBreaker) recordResult(success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	
	switch cb.state {
	case Closed:
		if success {
			cb.failures = 0
		} else {
			cb.failures++
			cb.lastFailureTime = time.Now()
			if cb.failures >= cb.maxFailures {
				cb.state = Open
			}
		}
		
	case HalfOpen:
		if success {
			cb.successes++
			if cb.successes >= cb.successThreshold {
				cb.state = Closed
				cb.failures = 0
				cb.successes = 0
			}
		} else {
			cb.state = Open
			cb.lastFailureTime = time.Now()
			cb.successes = 0
		}
	}
}

// State returns the current state
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Reset resets the circuit breaker
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	
	cb.state = Closed
	cb.failures = 0
	cb.successes = 0
	cb.halfOpenCurrent = 0
}
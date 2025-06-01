package actor_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/rollkit/rollkit/blockv2/actor"
)

func TestSupervisorStrategiesComprehensive(t *testing.T) {
	t.Run("AlwaysRestartSupervisor", func(t *testing.T) {
		supervisor := &actor.AlwaysRestartSupervisor{
			Delay: 10 * time.Millisecond,
		}

		decision := supervisor.HandleFailure(nil, assert.AnError)
		assert.Equal(t, actor.Restart, decision)
	})

	t.Run("OneForOneSupervisorWithinTimeWindow", func(t *testing.T) {
		supervisor := &actor.OneForOneSupervisor{
			MaxRestarts: 3,
			Within:      100 * time.Millisecond,
			Decider: func(err error) actor.Decision {
				return actor.Restart
			},
		}

		// Create a mock PID for testing
		mockPID := &actor.PID{}

		// First few restarts should work
		for i := 0; i < 3; i++ {
			decision := supervisor.HandleFailure(mockPID, assert.AnError)
			assert.Equal(t, actor.Restart, decision)
		}

		// Fourth restart within time window should escalate
		decision := supervisor.HandleFailure(mockPID, assert.AnError)
		assert.Equal(t, actor.Escalate, decision)
	})

	t.Run("OneForOneSupervisorTimeWindowReset", func(t *testing.T) {
		supervisor := &actor.OneForOneSupervisor{
			MaxRestarts: 2,
			Within:      50 * time.Millisecond,
			Decider: func(err error) actor.Decision {
				return actor.Restart
			},
		}

		// Create a mock PID for testing
		mockPID := &actor.PID{}

		// First restart
		decision := supervisor.HandleFailure(mockPID, assert.AnError)
		assert.Equal(t, actor.Restart, decision)

		// Wait for time window to reset
		time.Sleep(60 * time.Millisecond)

		// Should allow restart again after window reset
		decision = supervisor.HandleFailure(mockPID, assert.AnError)
		assert.Equal(t, actor.Restart, decision)
	})

	t.Run("OneForOneSupervisorDeciderStop", func(t *testing.T) {
		supervisor := &actor.OneForOneSupervisor{
			MaxRestarts: 5,
			Within:      time.Minute,
			Decider: func(err error) actor.Decision {
				// Custom logic to stop on certain errors
				if err.Error() == "fatal" {
					return actor.Stop
				}
				return actor.Restart
			},
		}

		// Create a mock PID for testing
		mockPID := &actor.PID{}

		// Normal error should restart
		decision := supervisor.HandleFailure(mockPID, assert.AnError)
		assert.Equal(t, actor.Restart, decision)

		// Fatal error should stop
		fatalErr := &fatalError{"fatal"}
		decision = supervisor.HandleFailure(mockPID, fatalErr)
		assert.Equal(t, actor.Stop, decision)
	})

	t.Run("OneForOneSupervisorDeciderResume", func(t *testing.T) {
		supervisor := &actor.OneForOneSupervisor{
			MaxRestarts: 3,
			Within:      time.Minute,
			Decider: func(err error) actor.Decision {
				if err.Error() == "temporary" {
					return actor.Resume
				}
				return actor.Restart
			},
		}

		// Create a mock PID for testing
		mockPID := &actor.PID{}

		// Temporary error should resume
		tempErr := &fatalError{"temporary"}
		decision := supervisor.HandleFailure(mockPID, tempErr)
		assert.Equal(t, actor.Resume, decision)
	})

	t.Run("DefaultSupervisor", func(t *testing.T) {
		supervisor := actor.DefaultSupervisor()
		assert.NotNil(t, supervisor)

		// Just test that we get a supervisor back
		// The actual behavior is tested in integration tests
		assert.NotNil(t, supervisor)
	})
}

func TestCircuitBreakerComprehensive(t *testing.T) {
	t.Run("CircuitBreakerStateTransitions", func(t *testing.T) {
		cb := actor.NewCircuitBreaker("test", 2, 100*time.Millisecond)

		// Should start closed
		assert.Equal(t, actor.Closed, cb.State())

		// First failure
		err := cb.Call(func() error {
			return assert.AnError
		})
		assert.Error(t, err)
		assert.Equal(t, actor.Closed, cb.State())

		// Second failure should open circuit
		err = cb.Call(func() error {
			return assert.AnError
		})
		assert.Error(t, err)
		assert.Equal(t, actor.Open, cb.State())

		// Should fail fast while open
		start := time.Now()
		err = cb.Call(func() error {
			time.Sleep(50 * time.Millisecond) // This shouldn't execute
			return nil
		})
		elapsed := time.Since(start)
		assert.Error(t, err)
		assert.Less(t, elapsed, 25*time.Millisecond)
		assert.Contains(t, err.Error(), "circuit breaker test is open")

		// Wait for half-open
		time.Sleep(110 * time.Millisecond)

		// Next call should be attempted (half-open state)
		executed := false
		err = cb.Call(func() error {
			executed = true
			return nil // Success
		})
		assert.NoError(t, err)
		assert.True(t, executed)
		assert.Equal(t, actor.Closed, cb.State())
	})

	t.Run("CircuitBreakerHalfOpenFailure", func(t *testing.T) {
		cb := actor.NewCircuitBreaker("half-open", 1, 50*time.Millisecond)

		// Open the circuit
		cb.Call(func() error { return assert.AnError })
		assert.Equal(t, actor.Open, cb.State())

		// Wait for half-open
		time.Sleep(60 * time.Millisecond)

		// Fail in half-open state
		err := cb.Call(func() error {
			return assert.AnError
		})
		assert.Error(t, err)
		assert.Equal(t, actor.Open, cb.State())
	})

	t.Run("CircuitBreakerConcurrency", func(t *testing.T) {
		cb := actor.NewCircuitBreaker("concurrent", 5, time.Minute)

		// Test concurrent access
		const numGoroutines = 10
		results := make(chan error, numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(i int) {
				err := cb.Call(func() error {
					if i%2 == 0 {
						return assert.AnError
					}
					return nil
				})
				results <- err
			}(i)
		}

		// Collect results
		successCount := 0
		for i := 0; i < numGoroutines; i++ {
			err := <-results
			if err == nil {
				successCount++
			}
		}

		assert.Greater(t, successCount, 0)
	})

	t.Run("CircuitBreakerZeroThreshold", func(t *testing.T) {
		cb := actor.NewCircuitBreaker("zero", 0, time.Minute)

		// Should immediately open on first failure
		err := cb.Call(func() error {
			return assert.AnError
		})
		assert.Error(t, err)
		assert.Equal(t, actor.Open, cb.State())
	})
}

func TestRateLimiterComprehensive(t *testing.T) {
	t.Run("RateLimiterTokenBucket", func(t *testing.T) {
		limiter := actor.NewRateLimiter("bucket", 3, 10) // 3 tokens, 10/sec

		// Should allow initial burst
		assert.True(t, limiter.AllowOne())
		assert.True(t, limiter.AllowOne())
		assert.True(t, limiter.AllowOne())

		// Should deny next request
		assert.False(t, limiter.AllowOne())

		// Wait for refill
		time.Sleep(150 * time.Millisecond)

		// Should allow one more
		assert.True(t, limiter.AllowOne())
	})

	t.Run("RateLimiterHighRate", func(t *testing.T) {
		limiter := actor.NewRateLimiter("high", 1000, 10000) // High rate

		// Should allow many requests
		for i := 0; i < 500; i++ {
			assert.True(t, limiter.AllowOne())
		}
	})

	t.Run("RateLimiterZeroCapacity", func(t *testing.T) {
		limiter := actor.NewRateLimiter("zero", 0, 100)

		// Should never allow requests
		assert.False(t, limiter.AllowOne())
		time.Sleep(50 * time.Millisecond)
		assert.False(t, limiter.AllowOne())
	})

	t.Run("RateLimiterConcurrency", func(t *testing.T) {
		limiter := actor.NewRateLimiter("concurrent", 100, 1000)

		const numGoroutines = 20
		results := make(chan bool, numGoroutines*10)

		for i := 0; i < numGoroutines; i++ {
			go func() {
				for j := 0; j < 10; j++ {
					results <- limiter.AllowOne()
				}
			}()
		}

		// Collect results
		allowedCount := 0
		for i := 0; i < numGoroutines*10; i++ {
			if <-results {
				allowedCount++
			}
		}

		// Should allow some but not all due to rate limiting
		assert.Greater(t, allowedCount, 50)
		assert.Less(t, allowedCount, numGoroutines*10)
	})
}

// Helper types for testing

type fatalError struct {
	msg string
}

func (f *fatalError) Error() string {
	return f.msg
}
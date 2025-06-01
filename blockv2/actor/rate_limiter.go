package actor

import (
	"fmt"
	"sync"
	"time"
)

// RateLimiter implements token bucket algorithm
type RateLimiter struct {
	mu           sync.Mutex
	name         string
	tokens       float64
	maxTokens    float64
	refillRate   float64
	lastRefill   time.Time
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(name string, maxTokens float64, refillRate float64) *RateLimiter {
	return &RateLimiter{
		name:       name,
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// Allow checks if an operation is allowed
func (rl *RateLimiter) Allow(tokens float64) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	
	rl.refill()
	
	if rl.tokens >= tokens {
		rl.tokens -= tokens
		return true
	}
	
	return false
}

// AllowOne checks if a single operation is allowed
func (rl *RateLimiter) AllowOne() bool {
	return rl.Allow(1)
}

// refill adds tokens based on time elapsed
func (rl *RateLimiter) refill() {
	now := time.Now()
	elapsed := now.Sub(rl.lastRefill).Seconds()
	tokensToAdd := elapsed * rl.refillRate
	
	rl.tokens = min(rl.tokens+tokensToAdd, rl.maxTokens)
	rl.lastRefill = now
}

// Tokens returns current token count
func (rl *RateLimiter) Tokens() float64 {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.refill()
	return rl.tokens
}

// MessageRateLimiter wraps an actor with rate limiting
type MessageRateLimiter struct {
	actor   Actor
	limiter *RateLimiter
}

// NewMessageRateLimiter creates a rate-limited actor wrapper
func NewMessageRateLimiter(actor Actor, messagesPerSecond float64) *MessageRateLimiter {
	return &MessageRateLimiter{
		actor:   actor,
		limiter: NewRateLimiter("message", messagesPerSecond, messagesPerSecond),
	}
}

// Receive implements Actor interface with rate limiting
func (m *MessageRateLimiter) Receive(ctx Context, msg any) {
	if !m.limiter.AllowOne() {
		ctx.Logger().Warn("message rate limit exceeded", 
			"actor", ctx.Self().Name(),
			"messageType", fmt.Sprintf("%T", msg))
		return
	}
	
	m.actor.Receive(ctx, msg)
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
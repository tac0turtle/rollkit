package actor

import (
	"sync/atomic"
)

// Metrics tracks actor performance metrics
type Metrics struct {
	Name             string
	MessagesSent     atomic.Int64
	MessagesReceived atomic.Int64
	DroppedMessages  atomic.Int64
	InvalidMessages  atomic.Int64
	ProcessingTime   *Histogram
	MailboxSize      atomic.Int32
	Restarts         atomic.Int32
	Panics           atomic.Int64
	Timeouts         atomic.Int64
}

// NewMetrics creates new metrics for an actor
func NewMetrics(name string) *Metrics {
	return &Metrics{
		Name:           name,
		ProcessingTime: NewHistogram(),
	}
}

// Histogram is a simple histogram implementation
type Histogram struct {
	mu     atomic.Value // stores *sync.Mutex
	values []float64
}

// NewHistogram creates a new histogram
func NewHistogram() *Histogram {
	return &Histogram{}
}

// Observe records a value
func (h *Histogram) Observe(v float64) {
	// Simplified - in production use prometheus or similar
}
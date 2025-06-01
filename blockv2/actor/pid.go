package actor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"cosmossdk.io/log"
)

// PID is a process identifier for an actor
type PID struct {
	name       string
	system     *System
	actor      Actor
	mailbox    chan Envelope
	ctx        context.Context
	cancel     context.CancelFunc
	logger     log.Logger
	supervisor SupervisorStrategy
	metrics    *Metrics

	// State management
	running    atomic.Bool
	restarting atomic.Bool
	restarts   atomic.Int32
	started    chan struct{} // Signal when actor is fully started
}

// Envelope wraps a message with metadata
type Envelope struct {
	Message   any
	Sender    *PID
	Timestamp time.Time
}

// Tell sends a fire-and-forget message to the actor
func (p *PID) Tell(msg any) error {
	return p.tell(msg, nil)
}

// Send sends a message from a specific sender
func (p *PID) Send(msg any, sender *PID) error {
	return p.tell(msg, sender)
}

func (p *PID) tell(msg any, sender *PID) error {
	// Check if actor is running (unless it's a system message)
	if !p.IsRunning() {
		switch msg.(type) {
		case Started, Stopping:
			// Allow system messages even when not running
		default:
			return fmt.Errorf("actor %s is not running", p.name)
		}
	}

	// Validate message if it implements Validatable
	if v, ok := msg.(Validatable); ok {
		if err := v.Validate(); err != nil {
			p.metrics.InvalidMessages.Add(1)
			return fmt.Errorf("message validation failed: %w", err)
		}
	}

	envelope := Envelope{
		Message:   msg,
		Sender:    sender,
		Timestamp: time.Now(),
	}

	select {
	case p.mailbox <- envelope:
		p.metrics.MessagesSent.Add(1)
		return nil
	default:
		p.metrics.DroppedMessages.Add(1)
		return fmt.Errorf("actor %s mailbox full", p.name)
	}
}

// Request sends a message and waits for a response
func (p *PID) Request(msg any, timeout time.Duration) (any, error) {
	reply := make(chan any, 1)
	req := &Request{
		Message: msg,
		Reply:   reply,
	}

	if err := p.Tell(req); err != nil {
		return nil, err
	}

	select {
	case resp := <-reply:
		if err, ok := resp.(error); ok {
			return nil, err
		}
		return resp, nil
	case <-time.After(timeout):
		p.metrics.Timeouts.Add(1)
		return nil, fmt.Errorf("request to %s timed out after %v", p.name, timeout)
	}
}

// Stop gracefully stops the actor
func (p *PID) Stop() {
	if p.running.Load() {
		p.Tell(Stopping{})
		p.cancel()
		
		// Wait for actor to finish processing
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()
		
		// Wait for running to become false or timeout
		for p.running.Load() {
			select {
			case <-timeout.C:
				// Force stop
				p.running.Store(false)
				return
			default:
				time.Sleep(time.Millisecond)
			}
		}
	}
}

// Name returns the actor's name
func (p *PID) Name() string {
	return p.name
}

// IsRunning returns whether the actor is running
func (p *PID) IsRunning() bool {
	return p.running.Load()
}

// run is the main actor loop
func (p *PID) run() {
	defer func() {
		p.running.Store(false)
		if r := recover(); r != nil {
			p.handlePanic(r)
		}
	}()

	// Set running state after goroutine starts
	p.running.Store(true)

	// Signal that actor is ready
	close(p.started)

	// Create actor context
	actorCtx := &actorContext{
		self:   p,
		system: p.system,
		logger: p.logger,
	}

	for {
		select {
		case envelope := <-p.mailbox:
			if !p.running.Load() {
				// Actor is stopping, don't process new messages
				return
			}
			p.processMessage(actorCtx, envelope)
		case <-p.ctx.Done():
			p.running.Store(false)
			return
		}
	}
}

func (p *PID) processMessage(ctx *actorContext, envelope Envelope) {
	ctx.sender = envelope.Sender
	ctx.message = envelope.Message

	p.metrics.MessagesReceived.Add(1)
	start := time.Now()

	defer func() {
		p.metrics.ProcessingTime.Observe(time.Since(start).Seconds())
		if r := recover(); r != nil {
			p.handleMessagePanic(ctx, r)
		}
	}()

	// Handle system messages
	switch msg := envelope.Message.(type) {
	case Started:
		p.logger.Debug("actor started")
		p.actor.Receive(ctx, msg)
	case Stopping:
		p.logger.Debug("actor stopping")
		p.actor.Receive(ctx, msg)
		p.running.Store(false)
		return
	case *Request:
		// Handle request-reply pattern
		func() {
			defer func() {
				if r := recover(); r != nil {
					msg.Reply <- fmt.Errorf("panic processing request: %v", r)
				}
			}()

			p.actor.Receive(ctx, msg.Message)
			// Actor should call ctx.Respond() to send reply
		}()
	default:
		p.actor.Receive(ctx, envelope.Message)
	}
}

func (p *PID) handlePanic(r any) {
	p.metrics.Panics.Add(1)
	p.logger.Error("actor panic", "panic", r)

	if p.supervisor != nil {
		decision := p.supervisor.HandleFailure(p, fmt.Errorf("panic: %v", r))
		p.applySupervisionDecision(decision)
	}
}

func (p *PID) handleMessagePanic(ctx *actorContext, r any) {
	p.metrics.Panics.Add(1)
	p.logger.Error("panic processing message",
		"panic", r,
		"messageType", fmt.Sprintf("%T", ctx.message))

	if p.supervisor != nil {
		decision := p.supervisor.HandleFailure(p, fmt.Errorf("message panic: %v", r))
		p.applySupervisionDecision(decision)
	}
}

func (p *PID) applySupervisionDecision(decision Decision) {
	switch decision {
	case Restart:
		p.restart()
	case Stop:
		p.Stop()
	case Resume:
		// Continue processing
	case Escalate:
		p.logger.Error("escalating failure to system")
		p.Stop()
	}
}

func (p *PID) restart() {
	if !p.restarting.CompareAndSwap(false, true) {
		return
	}
	defer p.restarting.Store(false)

	p.restarts.Add(1)
	p.logger.Info("restarting actor", "restarts", p.restarts.Load())

	// Clear mailbox
	for len(p.mailbox) > 0 {
		<-p.mailbox
	}

	// Create new started channel for restart
	p.started = make(chan struct{})

	// Restart the actor
	go p.run()

	// Wait for restart to complete
	<-p.started

	p.Tell(Started{})
}

// actorContext provides context for message processing
type actorContext struct {
	self    *PID
	sender  *PID
	system  *System
	logger  log.Logger
	message any

	mu      sync.Mutex
	replies []any
}

// Self returns the current actor's PID
func (c *actorContext) Self() *PID {
	return c.self
}

// Sender returns the message sender's PID
func (c *actorContext) Sender() *PID {
	return c.sender
}

// System returns the actor system
func (c *actorContext) System() *System {
	return c.system
}

// Logger returns the actor's logger
func (c *actorContext) Logger() log.Logger {
	return c.logger
}

// Message returns the current message being processed
func (c *actorContext) Message() any {
	return c.message
}

// Respond sends a response to a request
func (c *actorContext) Respond(msg any) {
	if req, ok := c.message.(*Request); ok {
		select {
		case req.Reply <- msg:
		default:
			c.logger.Error("failed to send reply: channel closed")
		}
	}
}

// Tell sends a message to another actor
func (c *actorContext) Tell(pid *PID, msg any) error {
	return pid.Send(msg, c.self)
}

// Context is the interface provided to actors during message processing
type Context interface {
	Self() *PID
	Sender() *PID
	System() *System
	Logger() log.Logger
	Message() any
	Respond(msg any)
	Tell(pid *PID, msg any) error
}

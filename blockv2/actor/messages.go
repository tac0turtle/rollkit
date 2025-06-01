package actor

// System messages
type (
	// Started is sent when an actor starts
	Started struct{}

	// Stopping is sent when an actor is stopping
	Stopping struct{}

	// Reboot is sent to restart an actor
	Reboot struct{}

	// HealthCheck is sent to check actor health
	HealthCheck struct{}
)

// Request wraps a message that expects a reply
type Request struct {
	Message any
	Reply   chan any
}

// Validatable messages can validate themselves
type Validatable interface {
	Validate() error
}

// TimerMessage is sent by timers
type TimerMessage struct {
	ID   string
	Data any
}

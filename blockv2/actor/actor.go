package actor

// Actor is the interface that all actors must implement
type Actor interface {
	// Receive processes incoming messages
	Receive(ctx Context, msg any)
}

// StatefulActor is an actor that can save and restore state
type StatefulActor interface {
	Actor
	
	// SaveState returns the current state to be persisted
	SaveState() ([]byte, error)
	
	// RestoreState restores the actor from persisted state
	RestoreState(state []byte) error
}
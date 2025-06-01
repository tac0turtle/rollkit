package actors

import (
	"errors"
	"time"

	coresequencer "github.com/rollkit/rollkit/core/sequencer"
	"github.com/rollkit/rollkit/types"
)

// Timer messages
type (
	// Tick is sent for regular block production
	Tick struct{}

	// LazyTick is sent for lazy block production
	LazyTick struct{}

	// RetrieveTick triggers DA retrieval
	RetrieveTick struct{}

	// HeaderStoreTick triggers header store sync
	HeaderStoreTick struct{}

	// DataStoreTick triggers data store sync
	DataStoreTick struct{}

	// ReapTick triggers transaction reaping
	ReapTick struct{}
)

// Event messages
type (
	// TxAvailable notifies that new transactions are available
	TxAvailable struct{}

	// HeaderFetched is emitted when a header is retrieved
	HeaderFetched struct {
		Header   *types.SignedHeader
		DAHeight uint64
	}

	// BatchFetched is emitted when batch data is retrieved
	BatchFetched struct {
		Data     *types.Data
		DAHeight uint64
	}

	// BlockProduced is emitted when a block is created
	BlockProduced struct {
		Header *types.SignedHeader
		Data   *types.Data
	}

	// DAIncluded is emitted when data is included in DA
	DAIncluded struct {
		Height uint64
		Hash   string
	}

	// StateAdvanced is emitted when state advances
	StateAdvanced struct {
		Height   uint64
		AppHash  []byte
		DAHeight uint64
	}
)

// Command messages
type (
	// SubmitHeaders requests header submission to DA
	SubmitHeaders struct {
		Headers []*types.SignedHeader
		Reply   chan error
	}

	// SubmitBatch requests batch submission to DA
	SubmitBatch struct {
		Batch coresequencer.Batch
		Reply chan error
	}

	// ProduceBlock requests block production
	ProduceBlock struct {
		Timestamp time.Time
		Reply     chan *ProduceBlockResult
	}

	// ValidateBlock requests block validation
	ValidateBlock struct {
		Header *types.SignedHeader
		Data   *types.Data
		Reply  chan error
	}

	// ApplyBlock requests block application
	ApplyBlock struct {
		Header *types.SignedHeader
		Data   *types.Data
		Reply  chan *ApplyBlockResult
	}

	// GetState requests current state
	GetState struct {
		Reply chan types.State
	}

	// UpdateState requests state update
	UpdateState struct {
		State types.State
		Reply chan error
	}
)

// Result types
type (
	// ProduceBlockResult contains block production results
	ProduceBlockResult struct {
		Header *types.SignedHeader
		Data   *types.Data
		Error  error
	}

	// ApplyBlockResult contains block application results
	ApplyBlockResult struct {
		NewState types.State
		Error    error
	}
)

// Query messages
type (
	// GetCacheEntry queries cache for an entry
	GetCacheEntry struct {
		Key   string
		Reply chan any
	}

	// SetCacheEntry sets a cache entry
	SetCacheEntry struct {
		Key   string
		Value any
		TTL   time.Duration
	}

	// GetPendingHeaders queries pending headers
	GetPendingHeaders struct {
		Reply chan []*types.SignedHeader
	}
)

// Validation for messages that need it
func (m HeaderFetched) Validate() error {
	if m.Header == nil {
		return errors.New("header cannot be nil")
	}
	return m.Header.ValidateBasic()
}

func (m BatchFetched) Validate() error {
	if m.Data == nil {
		return errors.New("data cannot be nil")
	}
	return nil
}

func (m BlockProduced) Validate() error {
	if m.Header == nil || m.Data == nil {
		return errors.New("header and data cannot be nil")
	}
	return m.Header.ValidateBasic()
}

func (m SubmitHeaders) Validate() error {
	if len(m.Headers) == 0 {
		return errors.New("no headers to submit")
	}
	if m.Reply == nil {
		return errors.New("reply channel cannot be nil")
	}
	for _, h := range m.Headers {
		if err := h.ValidateBasic(); err != nil {
			return err
		}
	}
	return nil
}

func (m SubmitBatch) Validate() error {
	if m.Reply == nil {
		return errors.New("reply channel cannot be nil")
	}
	return nil
}

func (m ValidateBlock) Validate() error {
	if m.Header == nil || m.Data == nil {
		return errors.New("header and data cannot be nil")
	}
	if m.Reply == nil {
		return errors.New("reply channel cannot be nil")
	}
	return nil
}

func (m ApplyBlock) Validate() error {
	if m.Header == nil || m.Data == nil {
		return errors.New("header and data cannot be nil")
	}
	if m.Reply == nil {
		return errors.New("reply channel cannot be nil")
	}
	return nil
}

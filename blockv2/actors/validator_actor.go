package actors

import (
	"bytes"
	"fmt"
	"time"

	"github.com/rollkit/rollkit/blockv2/actor"
	"github.com/rollkit/rollkit/types"
)

// ValidatorActor validates blocks and state transitions
type ValidatorActor struct {
	// Actor references
	stateActor *actor.PID

	// Configuration
	chainID         string
	proposerAddress []byte

	// Validation metrics
	validations     int64
	failures        int64
	lastFailureTime int64
}

// NewValidatorActor creates a new validator actor
func NewValidatorActor(
	chainID string,
	proposerAddress []byte,
	stateActor *actor.PID,
) *ValidatorActor {
	return &ValidatorActor{
		stateActor:      stateActor,
		chainID:         chainID,
		proposerAddress: proposerAddress,
	}
}

// Receive processes incoming messages
func (v *ValidatorActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case actor.Started:
		ctx.Logger().Info("validator actor started")

	case ValidateBlock:
		err := v.validateBlock(ctx, m.Header, m.Data)

		select {
		case m.Reply <- err:
		default:
			ctx.Logger().Error("failed to send validation reply")
		}

	case actor.HealthCheck:
		ctx.Logger().Info("validator health check",
			"validations", v.validations,
			"failures", v.failures)
	}
}

// validateBlock performs comprehensive block validation
func (v *ValidatorActor) validateBlock(ctx actor.Context, header *types.SignedHeader, data *types.Data) error {
	v.validations++

	// 1. Basic header validation
	if err := header.ValidateBasic(); err != nil {
		v.recordFailure()
		return fmt.Errorf("invalid header: %w", err)
	}

	// 2. Validate header against data
	if err := types.Validate(header, data); err != nil {
		v.recordFailure()
		return fmt.Errorf("header-data validation failed: %w", err)
	}

	// 3. Check chain ID
	if header.ChainID() != v.chainID {
		v.recordFailure()
		return fmt.Errorf("chain ID mismatch: expected %s, got %s", v.chainID, header.ChainID())
	}

	// 4. Get current state
	stateReply := make(chan types.State, 1)
	err := ctx.Tell(v.stateActor, GetState{Reply: stateReply})
	if err != nil {
		return fmt.Errorf("failed to get state: %w", err)
	}

	currentState := <-stateReply

	// 5. Validate height progression
	expectedHeight := currentState.LastBlockHeight + 1
	if header.Height() != expectedHeight {
		v.recordFailure()
		return fmt.Errorf("invalid height: expected %d, got %d", expectedHeight, header.Height())
	}

	// 6. Validate time progression
	headerTime := header.Time()
	if header.Height() > 1 && !headerTime.After(currentState.LastBlockTime) {
		v.recordFailure()
		return fmt.Errorf("block time not increasing: %v <= %v", headerTime, currentState.LastBlockTime)
	}

	// 7. Validate proposer
	if !v.isValidProposer(header) {
		v.recordFailure()
		return fmt.Errorf("invalid proposer: %X", header.ProposerAddress)
	}

	// 8. Validate signature
	if err := v.validateSignature(header); err != nil {
		v.recordFailure()
		return fmt.Errorf("invalid signature: %w", err)
	}

	// 9. Validate data
	if err := v.validateData(data, header.Height()); err != nil {
		v.recordFailure()
		return fmt.Errorf("invalid data: %w", err)
	}

	// 10. Validate app hash consistency (for blocks after genesis)
	if header.Height() > 1 && !bytes.Equal(header.AppHash, currentState.AppHash) {
		v.recordFailure()
		return fmt.Errorf("app hash mismatch: expected %X, got %X", currentState.AppHash, header.AppHash)
	}

	ctx.Logger().Debug("block validated successfully",
		"height", header.Height(),
		"hash", header.Hash().String())

	return nil
}

// isValidProposer checks if the block proposer is valid
func (v *ValidatorActor) isValidProposer(header *types.SignedHeader) bool {
	// For single sequencer mode, check against configured proposer
	return bytes.Equal(header.ProposerAddress, v.proposerAddress)
}

// validateSignature verifies the block signature
func (v *ValidatorActor) validateSignature(header *types.SignedHeader) error {
	// Verify the header was signed by the proposer
	if header.Signer.PubKey == nil {
		return fmt.Errorf("missing signer public key")
	}

	// Verify proposer address matches signer
	address := v.proposerAddress

	if !bytes.Equal(address, header.ProposerAddress) {
		return fmt.Errorf("signer address mismatch: %X != %X", address, header.ProposerAddress)
	}

	// Verify signature
	headerBytes, err := header.Header.MarshalBinary()
	if err != nil {
		return fmt.Errorf("failed to marshal header: %w", err)
	}

	ok, err := header.Signer.PubKey.Verify(headerBytes, header.Signature)
	if err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}
	if !ok {
		return fmt.Errorf("signature verification failed: invalid signature")
	}

	return nil
}

// validateData performs data-specific validation
func (v *ValidatorActor) validateData(data *types.Data, height uint64) error {
	// Validate metadata if present
	if data.Metadata != nil {
		if data.Metadata.Height != height {
			return fmt.Errorf("metadata height mismatch: %d != %d", data.Metadata.Height, height)
		}

		if data.Metadata.ChainID != v.chainID {
			return fmt.Errorf("metadata chain ID mismatch: %s != %s", data.Metadata.ChainID, v.chainID)
		}
	}

	// Validate transaction sizes
	for i, tx := range data.Txs {
		if len(tx) == 0 {
			return fmt.Errorf("empty transaction at index %d", i)
		}

		// Add max transaction size check
		const maxTxSize = 1024 * 1024 // 1MB
		if len(tx) > maxTxSize {
			return fmt.Errorf("transaction %d exceeds max size: %d > %d", i, len(tx), maxTxSize)
		}
	}

	return nil
}

// recordFailure updates failure metrics
func (v *ValidatorActor) recordFailure() {
	v.failures++
	v.lastFailureTime = time.Now().Unix()
}

// ValidationRules defines configurable validation rules
type ValidationRules struct {
	MaxTxSize        int
	MaxBlockSize     int
	MaxBlockTxs      int
	MinTimeDiff      time.Duration
	MaxTimeDiff      time.Duration
	AllowEmptyBlocks bool
}

// ExtendedValidatorActor adds configurable validation rules
type ExtendedValidatorActor struct {
	*ValidatorActor
	rules ValidationRules
}

// NewExtendedValidatorActor creates a validator with custom rules
func NewExtendedValidatorActor(
	chainID string,
	proposerAddress []byte,
	stateActor *actor.PID,
	rules ValidationRules,
) *ExtendedValidatorActor {
	return &ExtendedValidatorActor{
		ValidatorActor: NewValidatorActor(chainID, proposerAddress, stateActor),
		rules:          rules,
	}
}

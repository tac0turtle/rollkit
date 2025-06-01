package actors_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	"github.com/rollkit/rollkit/blockv2/actor"
	"github.com/rollkit/rollkit/blockv2/actors"
	"github.com/rollkit/rollkit/types"
)

func TestValidatorActor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.NewNopLogger()
	system := actor.NewSystem(ctx, logger)

	// Create mock state actor
	stateActor := &mockStateActor{
		state: types.State{
			ChainID:         "test-chain",
			LastBlockHeight: 10,
			LastBlockTime:   time.Now().Add(-time.Hour),
			AppHash:         []byte("app-hash"),
		},
	}
	statePID, err := system.Spawn("state", stateActor)
	require.NoError(t, err)

	// Create validator actor
	proposerAddr := []byte("proposer-address")
	validator := actors.NewValidatorActor("test-chain", proposerAddr, statePID)
	validatorPID, err := system.Spawn("validator", validator)
	require.NoError(t, err)

	t.Run("ValidBlock", func(t *testing.T) {
		// Create valid header
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  11,
					Time:    uint64(time.Now().UnixNano()),
				},
				AppHash:         []byte("app-hash"),
				ProposerAddress: proposerAddr,
			},
			Signer: types.Signer{
				PubKey: &mockPubKey{},
			},
			Signature: []byte("valid-signature"),
		}

		data := &types.Data{
			Txs: []types.Tx{[]byte("tx1"), []byte("tx2")},
		}

		// Validate block
		reply := make(chan error, 1)
		err := validatorPID.Tell(actors.ValidateBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for validation")
		}
	})

	t.Run("InvalidHeight", func(t *testing.T) {
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  13, // Skip height 12
					Time:    uint64(time.Now().UnixNano()),
				},
				ProposerAddress: proposerAddr,
			},
			Signer: types.Signer{
				PubKey: &mockPubKey{},
			},
			Signature: []byte("valid-signature"),
		}

		data := &types.Data{}

		reply := make(chan error, 1)
		err := validatorPID.Tell(actors.ValidateBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "invalid height")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for validation")
		}
	})

	t.Run("InvalidChainID", func(t *testing.T) {
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "wrong-chain",
					Height:  11,
					Time:    uint64(time.Now().UnixNano()),
				},
				ProposerAddress: proposerAddr,
			},
			Signer: types.Signer{
				PubKey: &mockPubKey{},
			},
			Signature: []byte("valid-signature"),
		}

		data := &types.Data{}

		reply := make(chan error, 1)
		err := validatorPID.Tell(actors.ValidateBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "chain ID mismatch")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for validation")
		}
	})

	t.Run("InvalidProposer", func(t *testing.T) {
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  11,
					Time:    uint64(time.Now().UnixNano()),
				},
				ProposerAddress: []byte("wrong-proposer"),
			},
			Signer: types.Signer{
				PubKey: &mockPubKey{},
			},
			Signature: []byte("valid-signature"),
		}

		data := &types.Data{}

		reply := make(chan error, 1)
		err := validatorPID.Tell(actors.ValidateBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "invalid proposer")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for validation")
		}
	})

	t.Run("InvalidSignature", func(t *testing.T) {
		header := &types.SignedHeader{
			Header: types.Header{
				BaseHeader: types.BaseHeader{
					ChainID: "test-chain",
					Height:  11,
					Time:    uint64(time.Now().UnixNano()),
				},
				ProposerAddress: proposerAddr,
			},
			Signer: types.Signer{
				PubKey: &mockPubKey{invalidSig: true},
			},
			Signature: []byte("invalid-signature"),
		}

		data := &types.Data{}

		reply := make(chan error, 1)
		err := validatorPID.Tell(actors.ValidateBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		})
		assert.NoError(t, err)

		select {
		case err := <-reply:
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "signature verification failed")
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for validation")
		}
	})
}

// Mock implementations

type mockStateActor struct {
	state types.State
}

func (m *mockStateActor) Receive(ctx actor.Context, msg any) {
	switch msg := msg.(type) {
	case actors.GetState:
		select {
		case msg.Reply <- m.state:
		default:
		}
	}
}

type mockPubKey struct {
	invalidSig bool
}

func (m *mockPubKey) Verify(msg []byte, sig []byte) (bool, error) {
	if m.invalidSig {
		return false, nil
	}
	return true, nil
}

func (m *mockPubKey) Bytes() []byte {
	return []byte("mock-pubkey")
}

func (m *mockPubKey) Equals(other interface{}) bool {
	return bytes.Equal(m.Bytes(), other.(*mockPubKey).Bytes())
}

func (m *mockPubKey) Type() string {
	return "mock"
}
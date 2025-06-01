package actors_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/rollkit/rollkit/blockv2/actors"
	coresequencer "github.com/rollkit/rollkit/core/sequencer"
	"github.com/rollkit/rollkit/types"
)

func TestMessages(t *testing.T) {
	t.Run("ValidateBlock", func(t *testing.T) {
		header := &types.SignedHeader{}
		data := &types.Data{}
		reply := make(chan error, 1)

		msg := actors.ValidateBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		}

		assert.Equal(t, header, msg.Header)
		assert.Equal(t, data, msg.Data)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("ProduceBlock", func(t *testing.T) {
		timestamp := time.Now()
		reply := make(chan *actors.ProduceBlockResult, 1)

		msg := actors.ProduceBlock{
			Timestamp: timestamp,
			Reply:     reply,
		}

		assert.Equal(t, timestamp, msg.Timestamp)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("ProduceBlockResult", func(t *testing.T) {
		header := &types.SignedHeader{}
		data := &types.Data{}
		err := assert.AnError

		result := &actors.ProduceBlockResult{
			Header: header,
			Data:   data,
			Error:  err,
		}

		assert.Equal(t, header, result.Header)
		assert.Equal(t, data, result.Data)
		assert.Equal(t, err, result.Error)
	})

	t.Run("ApplyBlock", func(t *testing.T) {
		header := &types.SignedHeader{}
		data := &types.Data{}
		reply := make(chan *actors.ApplyBlockResult, 1)

		msg := actors.ApplyBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		}

		assert.Equal(t, header, msg.Header)
		assert.Equal(t, data, msg.Data)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("ApplyBlockResult", func(t *testing.T) {
		newState := types.State{ChainID: "test"}
		err := assert.AnError

		result := &actors.ApplyBlockResult{
			NewState: newState,
			Error:    err,
		}

		assert.Equal(t, newState, result.NewState)
		assert.Equal(t, err, result.Error)
	})

	t.Run("GetState", func(t *testing.T) {
		reply := make(chan types.State, 1)

		msg := actors.GetState{
			Reply: reply,
		}

		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("UpdateState", func(t *testing.T) {
		state := types.State{ChainID: "test"}
		reply := make(chan error, 1)

		msg := actors.UpdateState{
			State: state,
			Reply: reply,
		}

		assert.Equal(t, state, msg.State)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("GetHeight", func(t *testing.T) {
		reply := make(chan uint64, 1)

		msg := actors.GetHeight{
			Reply: reply,
		}

		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("UpdateDAHeight", func(t *testing.T) {
		height := uint64(100)
		reply := make(chan error, 1)

		msg := actors.UpdateDAHeight{
			Height: height,
			Reply:  reply,
		}

		assert.Equal(t, height, msg.Height)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("SyncBlock", func(t *testing.T) {
		header := &types.SignedHeader{}
		data := &types.Data{}
		reply := make(chan error, 1)

		msg := actors.SyncBlock{
			Header: header,
			Data:   data,
			Reply:  reply,
		}

		assert.Equal(t, header, msg.Header)
		assert.Equal(t, data, msg.Data)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("SubmitHeaders", func(t *testing.T) {
		headers := []*types.SignedHeader{{}, {}}
		reply := make(chan error, 1)

		msg := actors.SubmitHeaders{
			Headers: headers,
			Reply:   reply,
		}

		assert.Equal(t, headers, msg.Headers)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("SubmitBatch", func(t *testing.T) {
		batch := coresequencer.Batch{
			Transactions: [][]byte{[]byte("tx1")},
		}
		reply := make(chan error, 1)

		msg := actors.SubmitBatch{
			Batch: batch,
			Reply: reply,
		}

		assert.Equal(t, batch, msg.Batch)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("TxAvailable", func(t *testing.T) {
		msg := actors.TxAvailable{}
		assert.NotNil(t, msg)
	})

	t.Run("SetCacheEntry", func(t *testing.T) {
		key := "test-key"
		value := "test-value"
		reply := make(chan error, 1)

		msg := actors.SetCacheEntry{
			Key:   key,
			Value: value,
			Reply: reply,
		}

		assert.Equal(t, key, msg.Key)
		assert.Equal(t, value, msg.Value)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("GetCacheEntry", func(t *testing.T) {
		key := "test-key"
		reply := make(chan any, 1)

		msg := actors.GetCacheEntry{
			Key:   key,
			Reply: reply,
		}

		assert.Equal(t, key, msg.Key)
		assert.Equal(t, reply, msg.Reply)
	})

	t.Run("DAIncluded", func(t *testing.T) {
		height := uint64(100)
		hash := "test-hash"

		msg := actors.DAIncluded{
			Height: height,
			Hash:   hash,
		}

		assert.Equal(t, height, msg.Height)
		assert.Equal(t, hash, msg.Hash)
	})

	t.Run("UpdatePendingHeaders", func(t *testing.T) {
		count := 42

		msg := &actors.UpdatePendingHeaders{
			Count: count,
		}

		assert.Equal(t, count, msg.Count)
	})
}

func TestConstants(t *testing.T) {
	// Test that constants are defined (they should be accessible)
	// This is more of a compile-time check
	t.Run("DataHashForEmptyTxs", func(t *testing.T) {
		// This tests that the constant is accessible and has the right type
		hash := actors.DataHashForEmptyTxs
		assert.NotNil(t, hash)
		assert.IsType(t, types.Hash{}, hash)
	})
}

func TestMessageValidation(t *testing.T) {
	t.Run("ValidatedMessage", func(t *testing.T) {
		// Test positive value
		msg := &actors.ValidatedMessage{Value: 10}
		err := msg.Validate()
		assert.NoError(t, err)

		// Test zero value
		msg = &actors.ValidatedMessage{Value: 0}
		err = msg.Validate()
		assert.NoError(t, err)

		// Test negative value
		msg = &actors.ValidatedMessage{Value: -1}
		err = msg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "value must be positive")
	})
}

func TestMessageChannelOperations(t *testing.T) {
	t.Run("NonBlockingChannelOperations", func(t *testing.T) {
		// Test that reply channels work correctly
		reply := make(chan error, 1)

		// Send to buffered channel should not block
		select {
		case reply <- nil:
			// Success
		default:
			t.Fatal("buffered channel should not block")
		}

		// Receive from channel
		select {
		case err := <-reply:
			assert.NoError(t, err)
		default:
			t.Fatal("should be able to receive from channel")
		}
	})

	t.Run("ChannelTimeout", func(t *testing.T) {
		reply := make(chan error)

		// Test timeout behavior
		select {
		case reply <- nil:
			t.Fatal("unbuffered channel should block")
		case <-time.After(10 * time.Millisecond):
			// Expected timeout
		}
	})
}
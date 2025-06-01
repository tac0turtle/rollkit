package actors

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/rollkit/rollkit/blockv2/actor"
	coresequencer "github.com/rollkit/rollkit/core/sequencer"
	"github.com/rollkit/rollkit/pkg/genesis"
	"github.com/rollkit/rollkit/pkg/signer"
	"github.com/rollkit/rollkit/pkg/store"
	"github.com/rollkit/rollkit/types"
)

// ProducerActor handles block creation
type ProducerActor struct {
	// Actor references
	stateActor     *actor.PID
	sequencerActor *actor.PID
	syncActor      *actor.PID
	submitterActor *actor.PID

	// Dependencies
	store     store.Store
	signer    signer.Signer
	genesis   genesis.Genesis
	sequencer coresequencer.Sequencer

	// Configuration
	maxPendingHeaders uint64

	// State
	lastBatchData  [][]byte
	pendingHeaders uint64
	producingBlock bool

	// Metrics
	blocksProduced int64
	emptyBlocks    int64
	failures       int64
}

// NewProducerActor creates a new producer actor
func NewProducerActor(
	store store.Store,
	signer signer.Signer,
	genesis genesis.Genesis,
	sequencer coresequencer.Sequencer,
	maxPendingHeaders uint64,
	stateActor *actor.PID,
	sequencerActor *actor.PID,
	syncActor *actor.PID,
	submitterActor *actor.PID,
) *ProducerActor {
	return &ProducerActor{
		stateActor:        stateActor,
		sequencerActor:    sequencerActor,
		syncActor:         syncActor,
		submitterActor:    submitterActor,
		store:             store,
		signer:            signer,
		genesis:           genesis,
		sequencer:         sequencer,
		maxPendingHeaders: maxPendingHeaders,
	}
}

// Receive processes incoming messages
func (p *ProducerActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case actor.Started:
		ctx.Logger().Info("producer actor started")
		p.loadLastBatchData(ctx)

	case ProduceBlock:
		p.handleProduceBlock(ctx, m)

	case *UpdatePendingHeaders:
		p.pendingHeaders = m.Count

	case actor.HealthCheck:
		ctx.Logger().Info("producer health",
			"blocksProduced", p.blocksProduced,
			"emptyBlocks", p.emptyBlocks,
			"failures", p.failures)
	}
}

// loadLastBatchData loads the last batch data from store
func (p *ProducerActor) loadLastBatchData(ctx actor.Context) {
	data, err := p.store.GetMetadata(context.Background(), "last_batch_data")
	if err != nil {
		ctx.Logger().Debug("no last batch data found")
		return
	}

	p.lastBatchData = bytesToBatchData(data)
	ctx.Logger().Info("loaded last batch data", "size", len(p.lastBatchData))
}

// handleProduceBlock handles block production request
func (p *ProducerActor) handleProduceBlock(ctx actor.Context, msg ProduceBlock) {
	result := p.produceBlock(ctx, msg.Timestamp)

	select {
	case msg.Reply <- result:
	default:
		ctx.Logger().Error("failed to send produce block reply")
	}
}

// produceBlock creates a new block
func (p *ProducerActor) produceBlock(ctx actor.Context, timestamp time.Time) *ProduceBlockResult {
	// Check pending headers limit
	if p.maxPendingHeaders > 0 && p.pendingHeaders >= p.maxPendingHeaders {
		return &ProduceBlockResult{
			Error: fmt.Errorf("pending headers limit reached: %d", p.pendingHeaders),
		}
	}

	// Get current state
	stateReply := make(chan types.State, 1)
	err := ctx.Tell(p.stateActor, GetState{Reply: stateReply})
	if err != nil {
		p.failures++
		return &ProduceBlockResult{Error: err}
	}

	currentState := <-stateReply
	currentHeight := currentState.LastBlockHeight
	newHeight := currentHeight + 1

	// Get last block data
	var lastSignature *types.Signature
	var lastHeaderHash types.Hash

	if newHeight <= p.genesis.InitialHeight {
		// Genesis block
		lastSignature = &types.Signature{}
	} else {
		// Get last block signature
		lastSignature, err = p.store.GetSignature(context.Background(), currentHeight)
		if err != nil {
			p.failures++
			return &ProduceBlockResult{
				Error: fmt.Errorf("failed to get last signature: %w", err),
			}
		}

		// Get last header hash
		lastHeader, _, err := p.store.GetBlockData(context.Background(), currentHeight)
		if err != nil {
			p.failures++
			return &ProduceBlockResult{
				Error: fmt.Errorf("failed to get last block: %w", err),
			}
		}
		lastHeaderHash = lastHeader.Hash()
	}

	// Check if block already exists at this height
	existingHeader, existingData, err := p.store.GetBlockData(context.Background(), newHeight)
	if err == nil {
		ctx.Logger().Info("using existing block", "height", newHeight)
		p.blocksProduced++
		return &ProduceBlockResult{
			Header: existingHeader,
			Data:   existingData,
		}
	}

	// Retrieve batch from sequencer
	batch, err := p.retrieveBatch(ctx)
	if err != nil && err != ErrNoBatch {
		p.failures++
		return &ProduceBlockResult{
			Error: fmt.Errorf("failed to retrieve batch: %w", err),
		}
	}

	// Create block
	header, data, err := p.createBlock(
		ctx,
		newHeight,
		timestamp,
		lastSignature,
		lastHeaderHash,
		currentState,
		batch,
	)
	if err != nil {
		p.failures++
		return &ProduceBlockResult{
			Error: fmt.Errorf("failed to create block: %w", err),
		}
	}

	// Get signature
	signature, err := p.getSignature(header.Header)
	if err != nil {
		p.failures++
		return &ProduceBlockResult{
			Error: fmt.Errorf("failed to sign block: %w", err),
		}
	}

	header.Signature = signature

	// Validate the created block
	validateReply := make(chan error, 1)
	err = ctx.Tell(p.stateActor.System().Get("validator"), ValidateBlock{
		Header: header,
		Data:   data,
		Reply:  validateReply,
	})
	if err != nil {
		p.failures++
		return &ProduceBlockResult{Error: err}
	}

	if err := <-validateReply; err != nil {
		p.failures++
		return &ProduceBlockResult{
			Error: fmt.Errorf("block validation failed: %w", err),
		}
	}

	// Apply block
	applyReply := make(chan *ApplyBlockResult, 1)
	err = ctx.Tell(p.syncActor, ApplyBlock{
		Header: header,
		Data:   data,
		Reply:  applyReply,
	})
	if err != nil {
		p.failures++
		return &ProduceBlockResult{Error: err}
	}

	applyResult := <-applyReply
	if applyResult.Error != nil {
		p.failures++
		return &ProduceBlockResult{Error: applyResult.Error}
	}

	// Add metadata to data
	data.Metadata = &types.Metadata{
		ChainID:      header.ChainID(),
		Height:       header.Height(),
		Time:         header.BaseHeader.Time,
		LastDataHash: types.Hash{}, // Set appropriately
	}

	// Save block
	if err := p.store.SaveBlockData(context.Background(), header, data, &signature); err != nil {
		p.failures++
		return &ProduceBlockResult{
			Error: fmt.Errorf("failed to save block: %w", err),
		}
	}

	// Update store height
	if err := p.store.SetHeight(context.Background(), newHeight); err != nil {
		p.failures++
		return &ProduceBlockResult{
			Error: fmt.Errorf("failed to update height: %w", err),
		}
	}

	// Update state
	updateReply := make(chan error, 1)
	err = ctx.Tell(p.stateActor, UpdateState{
		State: applyResult.NewState,
		Reply: updateReply,
	})
	if err != nil {
		p.failures++
		return &ProduceBlockResult{Error: err}
	}

	if err := <-updateReply; err != nil {
		p.failures++
		return &ProduceBlockResult{
			Error: fmt.Errorf("failed to update state: %w", err),
		}
	}

	// Broadcast header and data
	go p.broadcast(ctx, header, data)

	// Update metrics
	p.blocksProduced++
	if batch == nil || len(batch.Transactions) == 0 {
		p.emptyBlocks++
	}

	ctx.Logger().Info("block produced",
		"height", newHeight,
		"hash", header.Hash().String(),
		"txs", len(data.Txs))

	return &ProduceBlockResult{
		Header: header,
		Data:   data,
	}
}

// retrieveBatch gets the next batch from sequencer
func (p *ProducerActor) retrieveBatch(ctx actor.Context) (*BatchData, error) {
	req := coresequencer.GetNextBatchRequest{
		Id:            []byte(p.genesis.ChainID),
		LastBatchData: p.lastBatchData,
	}

	res, err := p.sequencer.GetNextBatch(context.Background(), req)
	if err != nil {
		return nil, err
	}

	if res == nil || res.Batch == nil {
		return nil, ErrNoBatch
	}

	// Update last batch data
	if err := p.store.SetMetadata(context.Background(), "last_batch_data",
		batchDataToBytes(res.BatchData)); err != nil {
		ctx.Logger().Error("failed to save last batch data", "error", err)
	}

	p.lastBatchData = res.BatchData

	if len(res.Batch.Transactions) == 0 {
		return &BatchData{
			Batch: res.Batch,
			Time:  res.Timestamp,
			Data:  res.BatchData,
		}, ErrNoBatch
	}

	return &BatchData{
		Batch: res.Batch,
		Time:  res.Timestamp,
		Data:  res.BatchData,
	}, nil
}

// createBlock creates a new block
func (p *ProducerActor) createBlock(
	ctx actor.Context,
	height uint64,
	timestamp time.Time,
	lastSignature *types.Signature,
	lastHeaderHash types.Hash,
	lastState types.State,
	batchData *BatchData,
) (*types.SignedHeader, *types.Data, error) {
	// Verify signer
	if p.signer == nil {
		return nil, nil, fmt.Errorf("signer is nil")
	}

	pubKey, err := p.signer.GetPublic()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get public key: %w", err)
	}

	address, err := p.signer.GetAddress()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get address: %w", err)
	}

	if !bytes.Equal(p.genesis.ProposerAddress, address) {
		return nil, nil, fmt.Errorf("proposer address mismatch: %X != %X",
			address, p.genesis.ProposerAddress)
	}

	// Create data
	data := &types.Data{
		Txs: make(types.Txs, 0),
	}

	isEmpty := batchData == nil || batchData.Batch == nil ||
		len(batchData.Batch.Transactions) == 0

	var blockTime time.Time
	if batchData != nil {
		blockTime = batchData.Time
	} else {
		blockTime = timestamp
	}

	// Create header
	header := &types.SignedHeader{
		Header: types.Header{
			Version: types.Version{
				Block: lastState.Version.Block,
				App:   lastState.Version.App,
			},
			BaseHeader: types.BaseHeader{
				ChainID: lastState.ChainID,
				Height:  height,
				Time:    uint64(blockTime.UnixNano()),
			},
			LastHeaderHash:  lastHeaderHash,
			ConsensusHash:   make(types.Hash, 32),
			AppHash:         lastState.AppHash,
			ProposerAddress: p.genesis.ProposerAddress,
		},
		Signature: *lastSignature,
		Signer: types.Signer{
			PubKey:  pubKey,
			Address: p.genesis.ProposerAddress,
		},
	}

	// Add transactions if not empty
	if !isEmpty {
		data.Txs = make(types.Txs, len(batchData.Batch.Transactions))
		for i, tx := range batchData.Batch.Transactions {
			data.Txs[i] = types.Tx(tx)
		}
		header.DataHash = data.DACommitment()
	} else {
		header.DataHash = dataHashForEmptyTxs
	}

	return header, data, nil
}

// getSignature signs the header
func (p *ProducerActor) getSignature(header types.Header) (types.Signature, error) {
	b, err := header.MarshalBinary()
	if err != nil {
		return nil, err
	}

	return p.signer.Sign(b)
}

// broadcast sends header and data to broadcast actors
func (p *ProducerActor) broadcast(ctx actor.Context, header *types.SignedHeader, data *types.Data) {
	// In real implementation, send to broadcaster actors
	ctx.Logger().Debug("broadcasting block",
		"height", header.Height(),
		"hash", header.Hash().String())
}

// Helper types and functions
type BatchData struct {
	*coresequencer.Batch
	Time time.Time
	Data [][]byte
}

var ErrNoBatch = fmt.Errorf("no batch available")

func batchDataToBytes(data [][]byte) []byte {
	// Simple encoding - in production use proper serialization
	total := 0
	for _, d := range data {
		total += len(d) + 4
	}

	result := make([]byte, 0, total)
	for _, d := range data {
		// Length prefix
		lenBytes := make([]byte, 4)
		lenBytes[0] = byte(len(d))
		lenBytes[1] = byte(len(d) >> 8)
		lenBytes[2] = byte(len(d) >> 16)
		lenBytes[3] = byte(len(d) >> 24)
		result = append(result, lenBytes...)
		result = append(result, d...)
	}

	return result
}

func bytesToBatchData(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}

	var result [][]byte
	offset := 0

	for offset < len(data) {
		if offset+4 > len(data) {
			break
		}

		length := int(data[offset]) |
			int(data[offset+1])<<8 |
			int(data[offset+2])<<16 |
			int(data[offset+3])<<24

		offset += 4

		if offset+length > len(data) {
			break
		}

		entry := make([]byte, length)
		copy(entry, data[offset:offset+length])
		result = append(result, entry)

		offset += length
	}

	return result
}

// Message types specific to ProducerActor
type UpdatePendingHeaders struct {
	Count uint64
}

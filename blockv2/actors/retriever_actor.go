package actors

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/rollkit/rollkit/blockv2/actor"
	coreda "github.com/rollkit/rollkit/core/da"
	"github.com/rollkit/rollkit/types"
	pb "github.com/rollkit/rollkit/types/pb/rollkit/v1"
)

const (
	daFetcherTimeout = 30 * time.Second
	daFetcherRetries = 10
)

// RetrieverActor fetches headers and data from the DA layer
type RetrieverActor struct {
	// Actor references
	syncActor *actor.PID

	// Dependencies
	da              coreda.DA
	chainID         string
	proposerAddress []byte

	// State
	daHeight       uint64
	retrieverTimer *time.Timer

	// Circuit breaker for DA operations
	daCircuitBreaker *actor.CircuitBreaker

	// Metrics
	headersRetrieved int64
	batchesRetrieved int64
	daErrors         int64
}

// NewRetrieverActor creates a new retriever actor
func NewRetrieverActor(
	da coreda.DA,
	chainID string,
	proposerAddress []byte,
	startDAHeight uint64,
	syncActor *actor.PID,
) *RetrieverActor {
	return &RetrieverActor{
		syncActor:        syncActor,
		da:               da,
		chainID:          chainID,
		proposerAddress:  proposerAddress,
		daHeight:         startDAHeight,
		daCircuitBreaker: actor.NewCircuitBreaker("da-retriever", 5, 60*time.Second),
	}
}

// Receive processes incoming messages
func (r *RetrieverActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case actor.Started:
		r.onStarted(ctx)

	case RetrieveTick:
		r.retrieveNext(ctx)

	case *BlobsFound:
		r.daHeight++
		// Immediately try to retrieve next
		ctx.Self().Tell(RetrieveTick{})

	case *UpdateDAHeight:
		r.daHeight = m.Height

	case actor.HealthCheck:
		ctx.Logger().Info("retriever health",
			"daHeight", r.daHeight,
			"headersRetrieved", r.headersRetrieved,
			"batchesRetrieved", r.batchesRetrieved,
			"daErrors", r.daErrors)

	case actor.Stopping:
		if r.retrieverTimer != nil {
			r.retrieverTimer.Stop()
		}
	}
}

// onStarted initializes the retriever
func (r *RetrieverActor) onStarted(ctx actor.Context) {
	ctx.Logger().Info("retriever actor started", "daHeight", r.daHeight)

	// Start retrieval timer
	r.retrieverTimer = time.AfterFunc(0, func() {
		ctx.Self().Tell(RetrieveTick{})
	})
}

// retrieveNext attempts to retrieve the next DA height
func (r *RetrieverActor) retrieveNext(ctx actor.Context) {
	err := r.processNextDABlock(ctx)
	if err != nil {
		if !r.isHeightFromFuture(err) {
			r.daErrors++
			ctx.Logger().Error("failed to retrieve from DA",
				"daHeight", r.daHeight,
				"error", err)
		}
		// Reset timer for next attempt
		r.retrieverTimer.Reset(time.Second)
	}
}

// processNextDABlock retrieves and processes data from DA
func (r *RetrieverActor) processNextDABlock(ctx actor.Context) error {
	ctx.Logger().Debug("retrieving from DA", "height", r.daHeight)

	var finalErr error

	// Retry with circuit breaker
	for attempt := 0; attempt < daFetcherRetries; attempt++ {
		var blobsResp coreda.ResultRetrieve

		err := r.daCircuitBreaker.Call(func() error {
			fetchCtx, cancel := context.WithTimeout(context.Background(), daFetcherTimeout)
			defer cancel()

			resp, err := r.da.Get(fetchCtx, []coreda.ID{coreda.ID(fmt.Sprintf("%d", r.daHeight))}, coreda.Namespace{})
			if resp.Code == coreda.StatusError {
				return fmt.Errorf("DA error: %s", resp.Message)
			}
			blobsResp = resp
			return nil
		})

		if err == nil {
			if blobsResp.Code == coreda.StatusNotFound {
				ctx.Logger().Debug("no data at DA height", "height", r.daHeight)
				return nil
			}

			// Process blobs
			r.processBlobs(ctx, blobsResp.Data)

			// Notify self that blobs were found
			ctx.Self().Tell(&BlobsFound{Height: r.daHeight})
			return nil
		}

		finalErr = errors.Join(finalErr, err)

		// Delay before retry
		select {
		case <-time.After(100 * time.Millisecond):
		default:
		}
	}

	return finalErr
}

// processBlobs handles retrieved blob data
func (r *RetrieverActor) processBlobs(ctx actor.Context, blobs [][]byte) {
	ctx.Logger().Debug("processing blobs", "count", len(blobs), "daHeight", r.daHeight)

	for _, blob := range blobs {
		if len(blob) == 0 {
			continue
		}

		// Try to decode as header
		if r.handlePotentialHeader(ctx, blob) {
			continue
		}

		// Try to decode as batch
		r.handlePotentialBatch(ctx, blob)
	}
}

// handlePotentialHeader tries to decode and process a header
func (r *RetrieverActor) handlePotentialHeader(ctx actor.Context, data []byte) bool {
	var headerPb pb.SignedHeader
	if err := proto.Unmarshal(data, &headerPb); err != nil {
		return false
	}

	header := new(types.SignedHeader)
	if err := header.FromProto(&headerPb); err != nil {
		ctx.Logger().Debug("failed to decode header", "error", err)
		return true
	}

	// Validate proposer
	if !r.isExpectedProposer(header) {
		ctx.Logger().Debug("ignoring header from unexpected proposer",
			"height", header.Height(),
			"proposer", fmt.Sprintf("%X", header.ProposerAddress))
		return true
	}

	// Send to sync actor
	err := ctx.Tell(r.syncActor, HeaderFetched{
		Header:   header,
		DAHeight: r.daHeight,
	})
	if err != nil {
		ctx.Logger().Error("failed to send header to sync", "error", err)
	} else {
		r.headersRetrieved++
		ctx.Logger().Info("header retrieved from DA",
			"height", header.Height(),
			"hash", header.Hash().String(),
			"daHeight", r.daHeight)
	}

	return true
}

// handlePotentialBatch tries to decode and process a batch
func (r *RetrieverActor) handlePotentialBatch(ctx actor.Context, data []byte) {
	var batchPb pb.Batch
	if err := proto.Unmarshal(data, &batchPb); err != nil {
		return
	}

	if len(batchPb.Txs) == 0 {
		return
	}

	// Convert to types.Data
	typeData := &types.Data{
		Txs: make(types.Txs, len(batchPb.Txs)),
	}
	for i, tx := range batchPb.Txs {
		typeData.Txs[i] = types.Tx(tx)
	}

	// Send to sync actor
	err := ctx.Tell(r.syncActor, BatchFetched{
		Data:     typeData,
		DAHeight: r.daHeight,
	})
	if err != nil {
		ctx.Logger().Error("failed to send batch to sync", "error", err)
	} else {
		r.batchesRetrieved++
		ctx.Logger().Info("batch retrieved from DA",
			"txCount", len(typeData.Txs),
			"hash", typeData.DACommitment().String(),
			"daHeight", r.daHeight)
	}
}

// isExpectedProposer checks if header is from expected sequencer
func (r *RetrieverActor) isExpectedProposer(header *types.SignedHeader) bool {
	return bytes.Equal(header.ProposerAddress, r.proposerAddress) &&
		header.ValidateBasic() == nil
}

// isHeightFromFuture checks if error indicates future height
func (r *RetrieverActor) isHeightFromFuture(err error) bool {
	if err == nil {
		return false
	}

	// Check for specific future height error
	futureErr := "given height is from the future"
	if strings.Contains(err.Error(), futureErr) {
		return true
	}

	// Check joined errors
	if joinedErr, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joinedErr.Unwrap() {
			if !r.isHeightFromFuture(e) {
				return false
			}
		}
		return true
	}

	return false
}

// Messages specific to RetrieverActor
type (
	// BlobsFound indicates blobs were found at a height
	BlobsFound struct {
		Height uint64
	}

	// UpdateDAHeight updates the current DA height
	UpdateDAHeight struct {
		Height uint64
	}
)

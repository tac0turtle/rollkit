package actors

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/rollkit/rollkit/blockv2/actor"
	coreda "github.com/rollkit/rollkit/core/da"
	coresequencer "github.com/rollkit/rollkit/core/sequencer"
	"github.com/rollkit/rollkit/pkg/store"
	"github.com/rollkit/rollkit/types"
	pb "github.com/rollkit/rollkit/types/pb/rollkit/v1"
)

const (
	maxSubmitAttempts = 30
	initialBackoff    = 100 * time.Millisecond
)

// DASubmitterActor handles submissions to the DA layer
type DASubmitterActor struct {
	// Actor references
	daIncluderActor *actor.PID

	// Dependencies
	da            coreda.DA
	store         store.Store
	gasPrice      float64
	gasMultiplier float64
	blockTime     time.Duration
	mempoolTTL    uint64

	// State - Headers
	pendingHeaders      []*types.SignedHeader
	lastSubmittedHeight uint64
	headerSubmitTimer   *time.Timer

	// State - Batches
	batchQueue       chan coresequencer.Batch
	batchSubmitTimer *time.Timer

	// Circuit breakers
	headerCircuitBreaker *actor.CircuitBreaker
	batchCircuitBreaker  *actor.CircuitBreaker

	// Metrics
	headersSubmitted int64
	batchesSubmitted int64
	submitFailures   int64
}

// NewDASubmitterActor creates a new DA submitter actor
func NewDASubmitterActor(
	da coreda.DA,
	store store.Store,
	gasPrice float64,
	gasMultiplier float64,
	blockTime time.Duration,
	mempoolTTL uint64,
	daIncluderActor *actor.PID,
) *DASubmitterActor {
	return &DASubmitterActor{
		daIncluderActor:      daIncluderActor,
		da:                   da,
		store:                store,
		gasPrice:             gasPrice,
		gasMultiplier:        gasMultiplier,
		blockTime:            blockTime,
		mempoolTTL:           mempoolTTL,
		batchQueue:           make(chan coresequencer.Batch, 100),
		headerCircuitBreaker: actor.NewCircuitBreaker("da-header", 3, 5*time.Minute),
		batchCircuitBreaker:  actor.NewCircuitBreaker("da-batch", 3, 5*time.Minute),
	}
}

// Receive processes incoming messages
func (s *DASubmitterActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case actor.Started:
		s.onStarted(ctx)

	case SubmitHeaders:
		s.handleSubmitHeaders(ctx, m)

	case SubmitBatch:
		s.handleSubmitBatch(ctx, m)

	case *HeaderSubmitTick:
		s.submitPendingHeaders(ctx)

	case *BatchSubmitTick:
		s.submitNextBatch(ctx)

	case actor.HealthCheck:
		ctx.Logger().Info("da submitter health",
			"pendingHeaders", len(s.pendingHeaders),
			"headersSubmitted", s.headersSubmitted,
			"batchesSubmitted", s.batchesSubmitted,
			"failures", s.submitFailures)

	case actor.Stopping:
		s.stopTimers()
	}
}

// onStarted initializes the submitter
func (s *DASubmitterActor) onStarted(ctx actor.Context) {
	ctx.Logger().Info("da submitter actor started")

	// Load last submitted height
	s.loadLastSubmittedHeight(ctx)

	// Start submission timers
	s.headerSubmitTimer = time.AfterFunc(s.blockTime, func() {
		ctx.Self().Tell(&HeaderSubmitTick{})
	})

	s.batchSubmitTimer = time.AfterFunc(s.blockTime, func() {
		ctx.Self().Tell(&BatchSubmitTick{})
	})
}

// loadLastSubmittedHeight loads the last submitted height from store
func (s *DASubmitterActor) loadLastSubmittedHeight(ctx actor.Context) {
	data, err := s.store.GetMetadata(context.Background(), "last_submitted_height")
	if err != nil {
		ctx.Logger().Debug("no last submitted height found")
		return
	}

	if len(data) == 8 {
		s.lastSubmittedHeight = binary.LittleEndian.Uint64(data)
		ctx.Logger().Info("loaded last submitted height", "height", s.lastSubmittedHeight)
	}
}

// handleSubmitHeaders queues headers for submission
func (s *DASubmitterActor) handleSubmitHeaders(ctx actor.Context, msg SubmitHeaders) {
	s.pendingHeaders = append(s.pendingHeaders, msg.Headers...)

	// Trigger immediate submission
	ctx.Self().Tell(&HeaderSubmitTick{})

	// Reply immediately
	select {
	case msg.Reply <- nil:
	default:
	}
}

// submitPendingHeaders submits headers to DA
func (s *DASubmitterActor) submitPendingHeaders(ctx actor.Context) {
	defer s.headerSubmitTimer.Reset(s.blockTime)

	if len(s.pendingHeaders) == 0 {
		return
	}

	// Get headers to submit
	height, err := s.store.Height(context.Background())
	if err != nil {
		ctx.Logger().Error("failed to get store height", "error", err)
		return
	}

	headersToSubmit := s.getHeadersToSubmit(ctx, height)
	if len(headersToSubmit) == 0 {
		return
	}

	// Submit with circuit breaker
	err = s.headerCircuitBreaker.Call(func() error {
		return s.doSubmitHeaders(ctx, headersToSubmit)
	})

	if err != nil {
		s.submitFailures++
		ctx.Logger().Error("failed to submit headers", "error", err)
	}
}

// doSubmitHeaders performs the actual header submission
func (s *DASubmitterActor) doSubmitHeaders(ctx actor.Context, headers []*types.SignedHeader) error {
	var (
		submittedCount int
		gasPrice       = s.gasPrice
		backoff        time.Duration
		attempt        = 0
	)

	for submittedCount < len(headers) && attempt < maxSubmitAttempts {
		if attempt > 0 {
			time.Sleep(backoff)
		}

		// Marshal headers
		headersBz := make([][]byte, 0, len(headers)-submittedCount)
		for i := submittedCount; i < len(headers); i++ {
			headerPb, err := headers[i].ToProto()
			if err != nil {
				return fmt.Errorf("failed to convert header to proto: %w", err)
			}

			data, err := proto.Marshal(headerPb)
			if err != nil {
				return fmt.Errorf("failed to marshal header: %w", err)
			}

			headersBz = append(headersBz, data)
		}

		// Submit to DA
		submitCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res := s.submitWithRetry(submitCtx, headersBz, gasPrice)
		cancel()

		switch res.Code {
		case coreda.StatusSuccess:
			submittedCount += int(res.SubmittedCount)
			s.headersSubmitted += int64(res.SubmittedCount)

			// Update last submitted height
			if res.SubmittedCount > 0 {
				lastHeight := headers[submittedCount-1].Height()
				s.updateLastSubmittedHeight(ctx, lastHeight)

				// Notify DA includer
				for i := 0; i < int(res.SubmittedCount); i++ {
					header := headers[i]
					ctx.Tell(s.daIncluderActor, DAIncluded{
						Height: header.Height(),
						Hash:   header.Hash().String(),
					})
				}
			}

			ctx.Logger().Info("submitted headers to DA",
				"count", res.SubmittedCount,
				"height", res.Height,
				"gasPrice", gasPrice)

			// Reset on success
			backoff = 0
			if s.gasMultiplier > 0 && gasPrice > s.gasPrice {
				gasPrice = gasPrice / s.gasMultiplier
			}

		case coreda.StatusNotIncludedInBlock, coreda.StatusAlreadyInMempool:
			backoff = s.blockTime * time.Duration(s.mempoolTTL)
			if s.gasMultiplier > 0 {
				gasPrice = gasPrice * s.gasMultiplier
			}

		default:
			backoff = s.exponentialBackoff(backoff)
		}

		attempt++
	}

	// Remove submitted headers
	if submittedCount > 0 {
		s.pendingHeaders = s.pendingHeaders[submittedCount:]
	}

	if submittedCount < len(headers) {
		return fmt.Errorf("only submitted %d of %d headers after %d attempts",
			submittedCount, len(headers), attempt)
	}

	return nil
}

// handleSubmitBatch queues a batch for submission
func (s *DASubmitterActor) handleSubmitBatch(ctx actor.Context, msg SubmitBatch) {
	select {
	case s.batchQueue <- msg.Batch:
		// Reply immediately
		select {
		case msg.Reply <- nil:
		default:
		}
	default:
		// Queue full
		err := fmt.Errorf("batch queue full")
		select {
		case msg.Reply <- err:
		default:
		}
	}
}

// submitNextBatch submits the next batch in queue
func (s *DASubmitterActor) submitNextBatch(ctx actor.Context) {
	defer s.batchSubmitTimer.Reset(s.blockTime)

	select {
	case batch := <-s.batchQueue:
		err := s.batchCircuitBreaker.Call(func() error {
			return s.doSubmitBatch(ctx, batch)
		})

		if err != nil {
			s.submitFailures++
			ctx.Logger().Error("failed to submit batch", "error", err)
		}
	default:
		// No batches to submit
	}
}

// doSubmitBatch performs the actual batch submission
func (s *DASubmitterActor) doSubmitBatch(ctx actor.Context, batch coresequencer.Batch) error {
	// Marshal batch
	batchPb := &pb.Batch{
		Txs: batch.Transactions,
	}

	batchBz, err := proto.Marshal(batchPb)
	if err != nil {
		return fmt.Errorf("failed to marshal batch: %w", err)
	}

	// Submit with retry logic similar to headers
	gasPrice := s.gasPrice
	var backoff time.Duration

	for attempt := 0; attempt < maxSubmitAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
		}

		submitCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res := s.submitWithRetry(submitCtx, [][]byte{batchBz}, gasPrice)
		cancel()

		switch res.Code {
		case coreda.StatusSuccess:
			s.batchesSubmitted++

			// Create types.Data for DA inclusion notification
			data := &types.Data{
				Txs: make(types.Txs, len(batch.Transactions)),
			}
			for i, tx := range batch.Transactions {
				data.Txs[i] = types.Tx(tx)
			}

			// Notify DA includer
			ctx.Tell(s.daIncluderActor, DAIncluded{
				Height: 0, // Batch doesn't have height
				Hash:   data.DACommitment().String(),
			})

			ctx.Logger().Info("submitted batch to DA",
				"txCount", len(batch.Transactions),
				"height", res.Height)

			return nil

		case coreda.StatusNotIncludedInBlock, coreda.StatusAlreadyInMempool:
			backoff = s.blockTime * time.Duration(s.mempoolTTL)
			if s.gasMultiplier > 0 {
				gasPrice = gasPrice * s.gasMultiplier
			}

		default:
			backoff = s.exponentialBackoff(backoff)
		}
	}

	return fmt.Errorf("failed to submit batch after %d attempts", maxSubmitAttempts)
}

// submitWithRetry wraps DA submission with options
func (s *DASubmitterActor) submitWithRetry(ctx context.Context, data [][]byte, gasPrice float64) coreda.ResultSubmit {
	// In production, use proper submit options
	ids := make([]coreda.ID, len(data))
	for i := range ids {
		ids[i] = coreda.ID(fmt.Sprintf("%d", i))
	}

	blobs := make([]coreda.Blob, len(data))
	for i, d := range data {
		blobs[i] = coreda.Blob(d)
	}

	return s.da.Submit(ctx, blobs, gasPrice, coreda.Namespace{})
}

// getHeadersToSubmit returns headers that need to be submitted
func (s *DASubmitterActor) getHeadersToSubmit(ctx actor.Context, currentHeight uint64) []*types.SignedHeader {
	var toSubmit []*types.SignedHeader

	for h := s.lastSubmittedHeight + 1; h <= currentHeight; h++ {
		header, _, err := s.store.GetBlockData(context.Background(), h)
		if err != nil {
			ctx.Logger().Error("failed to get block data", "height", h, "error", err)
			break
		}
		toSubmit = append(toSubmit, header)
	}

	return toSubmit
}

// updateLastSubmittedHeight updates the last submitted height
func (s *DASubmitterActor) updateLastSubmittedHeight(ctx actor.Context, height uint64) {
	s.lastSubmittedHeight = height

	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, height)

	if err := s.store.SetMetadata(context.Background(), "last_submitted_height", data); err != nil {
		ctx.Logger().Error("failed to save last submitted height", "error", err)
	}
}

// exponentialBackoff calculates exponential backoff duration
func (s *DASubmitterActor) exponentialBackoff(current time.Duration) time.Duration {
	if current == 0 {
		return initialBackoff
	}

	next := current * 2
	if next > s.blockTime {
		return s.blockTime
	}

	return next
}

// stopTimers stops all timers
func (s *DASubmitterActor) stopTimers() {
	if s.headerSubmitTimer != nil {
		s.headerSubmitTimer.Stop()
	}
	if s.batchSubmitTimer != nil {
		s.batchSubmitTimer.Stop()
	}
}

// Messages specific to DASubmitterActor
type (
	// HeaderSubmitTick triggers header submission
	HeaderSubmitTick struct{}

	// BatchSubmitTick triggers batch submission
	BatchSubmitTick struct{}
)

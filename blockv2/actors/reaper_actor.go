package actors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	ds "github.com/ipfs/go-datastore"

	"github.com/rollkit/rollkit/blockv2/actor"
	coreexecutor "github.com/rollkit/rollkit/core/execution"
	coresequencer "github.com/rollkit/rollkit/core/sequencer"
)

// ReaperActor periodically retrieves transactions from the executor
// and submits new ones to the sequencer
type ReaperActor struct {
	// Actor references
	aggregatorActor *actor.PID
	submitterActor  *actor.PID

	// Dependencies
	exec      coreexecutor.Executor
	sequencer coresequencer.Sequencer
	chainID   string
	interval  time.Duration
	seenStore ds.Batching

	// State
	reapTimer *time.Timer

	// Rate limiter for transaction submission
	rateLimiter *actor.RateLimiter

	// Metrics
	txsReaped    int64
	txsSubmitted int64
	txsSeen      int64
	reapErrors   int64
}

// NewReaperActor creates a new reaper actor
func NewReaperActor(
	exec coreexecutor.Executor,
	sequencer coresequencer.Sequencer,
	chainID string,
	interval time.Duration,
	seenStore ds.Batching,
	aggregatorActor *actor.PID,
	submitterActor *actor.PID,
) *ReaperActor {
	if interval <= 0 {
		interval = time.Second
	}

	return &ReaperActor{
		aggregatorActor: aggregatorActor,
		submitterActor:  submitterActor,
		exec:            exec,
		sequencer:       sequencer,
		chainID:         chainID,
		interval:        interval,
		seenStore:       seenStore,
		rateLimiter:     actor.NewRateLimiter("reaper", 100, 10), // 100 txs, 10/sec refill
	}
}

// Receive processes incoming messages
func (r *ReaperActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case actor.Started:
		r.onStarted(ctx)

	case ReapTick:
		r.reapTransactions(ctx)

	case *ForceReap:
		r.reapTransactions(ctx)

	case actor.HealthCheck:
		ctx.Logger().Info("reaper health",
			"txsReaped", r.txsReaped,
			"txsSubmitted", r.txsSubmitted,
			"txsSeen", r.txsSeen,
			"errors", r.reapErrors)

	case actor.Stopping:
		if r.reapTimer != nil {
			r.reapTimer.Stop()
		}
	}
}

// onStarted initializes the reaper
func (r *ReaperActor) onStarted(ctx actor.Context) {
	ctx.Logger().Info("reaper actor started", "interval", r.interval)

	// Start reap timer
	r.reapTimer = time.AfterFunc(0, func() {
		ctx.Self().Tell(ReapTick{})
	})
}

// reapTransactions retrieves and submits new transactions
func (r *ReaperActor) reapTransactions(ctx actor.Context) {
	defer r.reapTimer.Reset(r.interval)

	// Rate limit check
	if !r.rateLimiter.AllowOne() {
		ctx.Logger().Debug("reaper rate limited")
		return
	}

	// Get transactions from executor
	txs, err := r.exec.GetTxs(context.Background())
	if err != nil {
		r.reapErrors++
		ctx.Logger().Error("failed to get txs from executor", "error", err)
		return
	}

	r.txsReaped += int64(len(txs))

	if len(txs) == 0 {
		ctx.Logger().Debug("no transactions to reap")
		return
	}

	// Filter out already seen transactions
	newTxs, err := r.filterNewTransactions(ctx, txs)
	if err != nil {
		r.reapErrors++
		ctx.Logger().Error("failed to filter transactions", "error", err)
		return
	}

	if len(newTxs) == 0 {
		ctx.Logger().Debug("no new transactions after filtering")
		return
	}

	ctx.Logger().Debug("reaping transactions",
		"total", len(txs),
		"new", len(newTxs),
		"seen", len(txs)-len(newTxs))

	// Submit to sequencer
	if err := r.submitTransactions(ctx, newTxs); err != nil {
		r.reapErrors++
		ctx.Logger().Error("failed to submit transactions", "error", err)
		return
	}

	// Mark transactions as seen
	if err := r.markTransactionsSeen(ctx, newTxs); err != nil {
		ctx.Logger().Error("failed to mark transactions as seen", "error", err)
		// Don't return - transactions were submitted successfully
	}

	// Notify aggregator of new transactions
	if len(newTxs) > 0 {
		r.notifyNewTransactions(ctx)
	}
}

// filterNewTransactions filters out already seen transactions
func (r *ReaperActor) filterNewTransactions(ctx actor.Context, txs [][]byte) ([][]byte, error) {
	var newTxs [][]byte

	for _, tx := range txs {
		txHash := hashTx(tx)
		key := ds.NewKey(txHash)

		has, err := r.seenStore.Has(context.Background(), key)
		if err != nil {
			return nil, fmt.Errorf("failed to check seen store: %w", err)
		}

		if !has {
			newTxs = append(newTxs, tx)
		} else {
			r.txsSeen++
		}
	}

	return newTxs, nil
}

// submitTransactions submits transactions to the sequencer
func (r *ReaperActor) submitTransactions(ctx actor.Context, txs [][]byte) error {
	batch := &coresequencer.Batch{
		Transactions: txs,
	}

	req := coresequencer.SubmitBatchTxsRequest{
		Id:    []byte(r.chainID),
		Batch: batch,
	}

	_, err := r.sequencer.SubmitBatchTxs(context.Background(), req)
	if err != nil {
		return fmt.Errorf("sequencer submission failed: %w", err)
	}

	r.txsSubmitted += int64(len(txs))

	// Also submit to DA if submitter is available
	if r.submitterActor != nil {
		reply := make(chan error, 1)
		err := ctx.Tell(r.submitterActor, SubmitBatch{
			Batch: *batch,
			Reply: reply,
		})
		if err != nil {
			ctx.Logger().Error("failed to send batch to submitter", "error", err)
		} else {
			// Don't wait for reply - fire and forget
			go func() {
				select {
				case err := <-reply:
					if err != nil {
						ctx.Logger().Error("DA submission failed", "error", err)
					}
				case <-time.After(30 * time.Second):
					ctx.Logger().Warn("DA submission timed out")
				}
			}()
		}
	}

	return nil
}

// markTransactionsSeen persists seen transactions
func (r *ReaperActor) markTransactionsSeen(ctx actor.Context, txs [][]byte) error {
	batch, err := r.seenStore.Batch(context.Background())
	if err != nil {
		return fmt.Errorf("failed to create batch: %w", err)
	}

	for _, tx := range txs {
		txHash := hashTx(tx)
		key := ds.NewKey(txHash)
		if err := batch.Put(context.Background(), key, []byte{1}); err != nil {
			return fmt.Errorf("failed to put in batch: %w", err)
		}
	}

	if err := batch.Commit(context.Background()); err != nil {
		return fmt.Errorf("failed to commit batch: %w", err)
	}

	return nil
}

// notifyNewTransactions notifies the aggregator of new transactions
func (r *ReaperActor) notifyNewTransactions(ctx actor.Context) {
	if r.aggregatorActor != nil {
		if err := ctx.Tell(r.aggregatorActor, TxAvailable{}); err != nil {
			ctx.Logger().Error("failed to notify aggregator", "error", err)
		} else {
			ctx.Logger().Debug("notified aggregator of new transactions")
		}
	}
}

// hashTx computes the hash of a transaction
func hashTx(tx []byte) string {
	hash := sha256.Sum256(tx)
	return hex.EncodeToString(hash[:])
}

// Messages specific to ReaperActor
type (
	// ForceReap forces immediate transaction reaping
	ForceReap struct{}
)

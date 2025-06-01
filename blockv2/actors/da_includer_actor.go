package actors

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/rollkit/rollkit/blockv2/actor"
	coreexecutor "github.com/rollkit/rollkit/core/execution"
	"github.com/rollkit/rollkit/pkg/cache"
	"github.com/rollkit/rollkit/pkg/store"
	"github.com/rollkit/rollkit/types"
)

// DAIncluderActor tracks which blocks have been included in DA
// and updates the DA included height accordingly
type DAIncluderActor struct {
	// Actor references
	syncActor *actor.PID

	// Dependencies
	store store.Store
	exec  coreexecutor.Executor

	// Caches for tracking DA inclusion
	headerCache *cache.Cache[types.SignedHeader]
	dataCache   *cache.Cache[types.Data]

	// Current DA included height
	daIncludedHeight uint64
	mu               sync.RWMutex

	// Timer for periodic checks
	checkTimer    *time.Timer
	checkInterval time.Duration

	// Metrics
	heightsAdvanced int64
	checkAttempts   int64
}

// NewDAIncluderActor creates a new DA includer actor
func NewDAIncluderActor(
	store store.Store,
	exec coreexecutor.Executor,
	headerCache *cache.Cache[types.SignedHeader],
	dataCache *cache.Cache[types.Data],
	syncActor *actor.PID,
) *DAIncluderActor {
	return &DAIncluderActor{
		syncActor:     syncActor,
		store:         store,
		exec:          exec,
		headerCache:   headerCache,
		dataCache:     dataCache,
		checkInterval: 5 * time.Second,
	}
}

// Receive processes incoming messages
func (d *DAIncluderActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case actor.Started:
		d.onStarted(ctx)

	case DAIncluded:
		d.handleDAIncluded(ctx, m)

	case *CheckDAInclusion:
		d.checkForAdvancement(ctx)

	case *GetDAIncludedHeight:
		height := d.getDAIncludedHeight()
		select {
		case m.Reply <- height:
		default:
		}

	case actor.HealthCheck:
		ctx.Logger().Info("da includer health",
			"daIncludedHeight", d.getDAIncludedHeight(),
			"heightsAdvanced", d.heightsAdvanced,
			"checkAttempts", d.checkAttempts)

	case actor.Stopping:
		if d.checkTimer != nil {
			d.checkTimer.Stop()
		}
	}
}

// onStarted initializes the DA includer
func (d *DAIncluderActor) onStarted(ctx actor.Context) {
	// Load DA included height from store
	if height, err := d.store.GetMetadata(context.Background(), "da_included_height"); err == nil && len(height) == 8 {
		d.mu.Lock()
		d.daIncludedHeight = binary.LittleEndian.Uint64(height)
		d.mu.Unlock()
		ctx.Logger().Info("loaded DA included height", "height", d.daIncludedHeight)
	}

	// Start periodic check timer
	d.checkTimer = time.AfterFunc(d.checkInterval, func() {
		ctx.Self().Tell(&CheckDAInclusion{})
	})

	ctx.Logger().Info("da includer actor started", "checkInterval", d.checkInterval)
}

// handleDAIncluded marks a block/data as included in DA
func (d *DAIncluderActor) handleDAIncluded(ctx actor.Context, msg DAIncluded) {
	ctx.Logger().Debug("marking as DA included",
		"height", msg.Height,
		"hash", msg.Hash)

	// Mark in caches
	if msg.Height > 0 {
		d.headerCache.SetDAIncluded(msg.Hash)
	} else {
		d.dataCache.SetDAIncluded(msg.Hash)
	}

	// Trigger check for advancement
	ctx.Self().Tell(&CheckDAInclusion{})
}

// checkForAdvancement checks if we can advance the DA included height
func (d *DAIncluderActor) checkForAdvancement(ctx actor.Context) {
	defer d.checkTimer.Reset(d.checkInterval)

	d.checkAttempts++
	currentHeight := d.getDAIncludedHeight()
	advanced := false

	// Try to advance as far as possible
	for {
		nextHeight := currentHeight + 1

		// Check if next height is DA included
		isIncluded, err := d.isHeightDAIncluded(ctx, nextHeight)
		if err != nil {
			ctx.Logger().Debug("height not available for DA check",
				"height", nextHeight,
				"error", err)
			break
		}

		if !isIncluded {
			ctx.Logger().Debug("height not yet DA included", "height", nextHeight)
			break
		}

		// Advance the height
		if err := d.incrementDAIncludedHeight(ctx, nextHeight); err != nil {
			ctx.Logger().Error("failed to increment DA included height",
				"height", nextHeight,
				"error", err)
			break
		}

		currentHeight = nextHeight
		advanced = true
		d.heightsAdvanced++

		ctx.Logger().Info("advanced DA included height", "newHeight", currentHeight)
	}

	if advanced {
		// Notify sync actor of progress
		ctx.Tell(d.syncActor, &DAProgressUpdate{
			NewDAIncludedHeight: currentHeight,
		})
	}
}

// isHeightDAIncluded checks if a height has been included in DA
func (d *DAIncluderActor) isHeightDAIncluded(ctx actor.Context, height uint64) (bool, error) {
	// Get block data from store
	header, data, err := d.store.GetBlockData(context.Background(), height)
	if err != nil {
		return false, fmt.Errorf("failed to get block data: %w", err)
	}

	// Check header inclusion
	headerHash := header.Hash().String()
	if !d.headerCache.IsDAIncluded(headerHash) {
		return false, nil
	}

	// Check data inclusion (handle empty blocks)
	dataHash := data.DACommitment()
	if bytes.Equal(dataHash, dataHashForEmptyTxs) {
		// Empty blocks are always considered included
		return true, nil
	}

	return d.dataCache.IsDAIncluded(dataHash.String()), nil
}

// incrementDAIncludedHeight updates the DA included height
func (d *DAIncluderActor) incrementDAIncludedHeight(ctx actor.Context, newHeight uint64) error {
	// Update executor finality
	if err := d.exec.SetFinal(context.Background(), newHeight); err != nil {
		return fmt.Errorf("failed to set final height: %w", err)
	}

	// Persist to store
	heightBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(heightBytes, newHeight)

	if err := d.store.SetMetadata(context.Background(), "da_included_height", heightBytes); err != nil {
		return fmt.Errorf("failed to persist DA included height: %w", err)
	}

	// Update in memory
	d.mu.Lock()
	d.daIncludedHeight = newHeight
	d.mu.Unlock()

	return nil
}

// getDAIncludedHeight returns the current DA included height
func (d *DAIncluderActor) getDAIncludedHeight() uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.daIncludedHeight
}

// Messages specific to DAIncluderActor
type (
	// CheckDAInclusion triggers a check for DA height advancement
	CheckDAInclusion struct{}

	// GetDAIncludedHeight queries the current DA included height
	GetDAIncludedHeight struct {
		Reply chan uint64
	}

	// DAProgressUpdate notifies of DA included height progress
	DAProgressUpdate struct {
		NewDAIncludedHeight uint64
	}
)

package actors

import (
	"time"

	"github.com/rollkit/rollkit/blockv2/actor"
	"github.com/rollkit/rollkit/pkg/config"
	"github.com/rollkit/rollkit/pkg/genesis"
	"github.com/rollkit/rollkit/types"
)

// AggregatorActor handles block production timing and coordination
type AggregatorActor struct {
	// Actor references
	producerActor *actor.PID
	stateActor    *actor.PID

	// Configuration
	config    config.Config
	genesis   genesis.Genesis
	lazyMode  bool
	blockTime time.Duration
	lazyTime  time.Duration

	// Timers
	blockTimer *time.Timer
	lazyTimer  *time.Timer

	// State
	txsAvailable   bool
	lastBlockTime  time.Time
	producingBlock bool
}

// NewAggregatorActor creates a new aggregator actor
func NewAggregatorActor(
	config config.Config,
	genesis genesis.Genesis,
	producerActor *actor.PID,
	stateActor *actor.PID,
) *AggregatorActor {
	return &AggregatorActor{
		producerActor: producerActor,
		stateActor:    stateActor,
		config:        config,
		genesis:       genesis,
		lazyMode:      config.Node.LazyMode,
		blockTime:     config.Node.BlockTime.Duration,
		lazyTime:      config.Node.LazyBlockInterval.Duration,
	}
}

// Receive processes incoming messages
func (a *AggregatorActor) Receive(ctx actor.Context, msg any) {
	switch m := msg.(type) {
	case actor.Started:
		a.onStarted(ctx)

	case TxAvailable:
		a.txsAvailable = true
		ctx.Logger().Debug("transactions available")

	case Tick:
		a.onBlockTick(ctx)

	case LazyTick:
		a.onLazyTick(ctx)

	case ProduceBlock:
		a.handleProduceBlock(ctx, m)

	case BlockProduced:
		a.onBlockProduced(ctx, m)

	case actor.Stopping:
		a.stopTimers()
	}
}

// onStarted initializes the aggregator
func (a *AggregatorActor) onStarted(ctx actor.Context) {
	ctx.Logger().Info("aggregator actor started",
		"lazyMode", a.lazyMode,
		"blockTime", a.blockTime,
		"lazyTime", a.lazyTime)

	// Get current state to determine initial delay
	stateReply := make(chan types.State, 1)
	err := ctx.Tell(a.stateActor, GetState{Reply: stateReply})
	if err != nil {
		ctx.Logger().Error("failed to get initial state", "error", err)
		return
	}

	state := <-stateReply

	// Calculate initial delay
	var delay time.Duration
	if state.LastBlockHeight < a.genesis.InitialHeight {
		delay = time.Until(a.genesis.GenesisDAStartTime.Add(a.blockTime))
	} else {
		delay = time.Until(state.LastBlockTime.Add(a.blockTime))
	}

	if delay > 0 {
		ctx.Logger().Info("waiting to produce first block", "delay", delay)
		time.Sleep(delay)
	}

	// Start timers
	a.startTimers(ctx)
}

// startTimers initializes the block production timers
func (a *AggregatorActor) startTimers(ctx actor.Context) {
	// Block timer
	a.blockTimer = time.AfterFunc(0, func() {
		if err := ctx.Self().Tell(Tick{}); err != nil {
			ctx.Logger().Error("failed to send tick", "error", err)
		}
	})

	// Lazy timer (only in lazy mode)
	if a.lazyMode {
		a.lazyTimer = time.AfterFunc(a.lazyTime, func() {
			if err := ctx.Self().Tell(LazyTick{}); err != nil {
				ctx.Logger().Error("failed to send lazy tick", "error", err)
			}
		})
	}
}

// stopTimers stops all timers
func (a *AggregatorActor) stopTimers() {
	if a.blockTimer != nil {
		a.blockTimer.Stop()
	}
	if a.lazyTimer != nil {
		a.lazyTimer.Stop()
	}
}

// onBlockTick handles regular block timer
func (a *AggregatorActor) onBlockTick(ctx actor.Context) {
	if a.lazyMode {
		if a.txsAvailable {
			a.produceBlock(ctx, "block_timer")
			a.txsAvailable = false
		} else {
			// Just reset the timer
			a.blockTimer.Reset(a.blockTime)
		}
	} else {
		// Normal mode: always produce blocks
		a.produceBlock(ctx, "block_timer")
	}
}

// onLazyTick handles lazy timer
func (a *AggregatorActor) onLazyTick(ctx actor.Context) {
	ctx.Logger().Debug("lazy timer triggered")
	a.produceBlock(ctx, "lazy_timer")
}

// produceBlock triggers block production
func (a *AggregatorActor) produceBlock(ctx actor.Context, trigger string) {
	if a.producingBlock {
		ctx.Logger().Debug("already producing block, skipping")
		return
	}

	a.producingBlock = true
	start := time.Now()

	ctx.Logger().Debug("requesting block production", "trigger", trigger)

	// Send produce block request
	reply := make(chan *ProduceBlockResult, 1)
	err := ctx.Tell(a.producerActor, ProduceBlock{
		Timestamp: time.Now(),
		Reply:     reply,
	})

	if err != nil {
		ctx.Logger().Error("failed to send produce block message", "error", err)
		a.producingBlock = false
		a.resetTimers(start)
		return
	}

	// Wait for result asynchronously
	go func() {
		select {
		case result := <-reply:
			if result.Error != nil {
				ctx.Logger().Error("block production failed", "error", result.Error)
			} else {
				// Notify ourselves of successful production
				ctx.Self().Tell(BlockProduced{
					Header: result.Header,
					Data:   result.Data,
				})
			}
		case <-time.After(30 * time.Second):
			ctx.Logger().Error("block production timed out")
		}

		a.producingBlock = false
		a.resetTimers(start)
	}()
}

// onBlockProduced handles successful block production
func (a *AggregatorActor) onBlockProduced(ctx actor.Context, msg BlockProduced) {
	ctx.Logger().Info("block produced",
		"height", msg.Header.Height(),
		"txs", len(msg.Data.Txs))

	a.lastBlockTime = msg.Header.Time()
}

// resetTimers resets production timers
func (a *AggregatorActor) resetTimers(start time.Time) {
	elapsed := time.Since(start)

	// Reset block timer
	if a.blockTimer != nil {
		remaining := a.blockTime - elapsed
		if remaining < time.Millisecond {
			remaining = time.Millisecond
		}
		a.blockTimer.Reset(remaining)
	}

	// Reset lazy timer
	if a.lazyTimer != nil {
		remaining := a.lazyTime - elapsed
		if remaining < time.Millisecond {
			remaining = time.Millisecond
		}
		a.lazyTimer.Reset(remaining)
	}
}

// handleProduceBlock handles external block production requests
func (a *AggregatorActor) handleProduceBlock(ctx actor.Context, msg ProduceBlock) {
	// Forward to producer
	err := ctx.Tell(a.producerActor, msg)
	if err != nil {
		msg.Reply <- &ProduceBlockResult{Error: err}
	}
}

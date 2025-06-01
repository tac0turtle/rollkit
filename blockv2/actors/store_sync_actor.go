package actors

import (
	"bytes"
	"context"
	"fmt"
	"time"

	goheader "github.com/celestiaorg/go-header"

	"github.com/rollkit/rollkit/blockv2/actor"
	"github.com/rollkit/rollkit/pkg/store"
	"github.com/rollkit/rollkit/types"
)

// StoreSyncActor retrieves headers and data from P2P stores
type StoreSyncActor struct {
	// Actor references
	syncActor      *actor.PID
	validatorActor *actor.PID

	// Dependencies
	headerStore     goheader.Store[*types.SignedHeader]
	dataStore       goheader.Store[*types.Data]
	store           store.Store
	proposerAddress []byte

	// State
	lastHeaderStoreHeight uint64
	lastDataStoreHeight   uint64
	headerSyncTimer       *time.Timer
	dataSyncTimer         *time.Timer
	syncInterval          time.Duration

	// Rate limiters for store queries
	headerRateLimiter *actor.RateLimiter
	dataRateLimiter   *actor.RateLimiter

	// Metrics
	headersRetrieved int64
	dataRetrieved    int64
	syncErrors       int64
}

// NewStoreSyncActor creates a new store sync actor
func NewStoreSyncActor(
	headerStore goheader.Store[*types.SignedHeader],
	dataStore goheader.Store[*types.Data],
	store store.Store,
	proposerAddress []byte,
	syncActor *actor.PID,
	validatorActor *actor.PID,
) *StoreSyncActor {
	return &StoreSyncActor{
		syncActor:         syncActor,
		validatorActor:    validatorActor,
		headerStore:       headerStore,
		dataStore:         dataStore,
		store:             store,
		proposerAddress:   proposerAddress,
		syncInterval:      time.Second,
		headerRateLimiter: actor.NewRateLimiter("header-sync", 100, 10),
		dataRateLimiter:   actor.NewRateLimiter("data-sync", 100, 10),
	}
}

// Receive processes incoming messages
func (s *StoreSyncActor) Receive(ctx actor.Context, msg any) {
	switch msg.(type) {
	case actor.Started:
		s.onStarted(ctx)

	case HeaderStoreTick:
		s.syncHeaders(ctx)

	case DataStoreTick:
		s.syncData(ctx)

	case actor.HealthCheck:
		ctx.Logger().Info("store sync health",
			"lastHeaderHeight", s.lastHeaderStoreHeight,
			"lastDataHeight", s.lastDataStoreHeight,
			"headersRetrieved", s.headersRetrieved,
			"dataRetrieved", s.dataRetrieved,
			"errors", s.syncErrors)

	case actor.Stopping:
		s.stopTimers()
	}
}

// onStarted initializes the store sync actor
func (s *StoreSyncActor) onStarted(ctx actor.Context) {
	// Get initial heights
	var err error
	s.lastHeaderStoreHeight, err = s.store.Height(context.Background())
	if err != nil {
		ctx.Logger().Error("failed to get initial header height", "error", err)
	}

	s.lastDataStoreHeight = s.lastHeaderStoreHeight

	ctx.Logger().Info("store sync actor started",
		"headerHeight", s.lastHeaderStoreHeight,
		"dataHeight", s.lastDataStoreHeight)

	// Start sync timers
	s.headerSyncTimer = time.AfterFunc(0, func() {
		ctx.Self().Tell(HeaderStoreTick{})
	})

	s.dataSyncTimer = time.AfterFunc(time.Millisecond*100, func() {
		ctx.Self().Tell(DataStoreTick{})
	})
}

// syncHeaders retrieves new headers from the header store
func (s *StoreSyncActor) syncHeaders(ctx actor.Context) {
	defer s.headerSyncTimer.Reset(s.syncInterval)

	// Rate limit check
	if !s.headerRateLimiter.AllowOne() {
		return
	}

	// Get current header store height
	headerStoreHeight := s.headerStore.Height()
	if headerStoreHeight <= s.lastHeaderStoreHeight {
		return
	}

	ctx.Logger().Debug("syncing headers from store",
		"from", s.lastHeaderStoreHeight+1,
		"to", headerStoreHeight)

	// Retrieve headers
	headers, err := s.getHeadersFromStore(ctx, s.lastHeaderStoreHeight+1, headerStoreHeight)
	if err != nil {
		s.syncErrors++
		ctx.Logger().Error("failed to get headers from store",
			"from", s.lastHeaderStoreHeight+1,
			"to", headerStoreHeight,
			"error", err)
		return
	}

	// Get current DA height for context
	daHeight := uint64(0) // In production, get from DA actor

	// Process headers
	for _, header := range headers {
		// Validate proposer
		if !s.isExpectedProposer(header) {
			ctx.Logger().Debug("skipping header from unexpected proposer",
				"height", header.Height(),
				"proposer", fmt.Sprintf("%X", header.ProposerAddress))
			continue
		}

		// Send to sync actor
		err := ctx.Tell(s.syncActor, HeaderFetched{
			Header:   header,
			DAHeight: daHeight,
		})
		if err != nil {
			ctx.Logger().Error("failed to send header to sync", "error", err)
			break
		}

		s.headersRetrieved++
		ctx.Logger().Debug("header retrieved from p2p store",
			"height", header.Height(),
			"hash", header.Hash().String())
	}

	s.lastHeaderStoreHeight = headerStoreHeight
}

// syncData retrieves new data from the data store
func (s *StoreSyncActor) syncData(ctx actor.Context) {
	defer s.dataSyncTimer.Reset(s.syncInterval)

	// Rate limit check
	if !s.dataRateLimiter.AllowOne() {
		return
	}

	// Get current data store height
	dataStoreHeight := s.dataStore.Height()
	if dataStoreHeight <= s.lastDataStoreHeight {
		return
	}

	ctx.Logger().Debug("syncing data from store",
		"from", s.lastDataStoreHeight+1,
		"to", dataStoreHeight)

	// Retrieve data
	dataList, err := s.getDataFromStore(ctx, s.lastDataStoreHeight+1, dataStoreHeight)
	if err != nil {
		s.syncErrors++
		ctx.Logger().Error("failed to get data from store",
			"from", s.lastDataStoreHeight+1,
			"to", dataStoreHeight,
			"error", err)
		return
	}

	// Get current DA height for context
	daHeight := uint64(0) // In production, get from DA actor

	// Process data
	for _, data := range dataList {
		// Basic validation
		if data.Metadata == nil {
			ctx.Logger().Debug("skipping data without metadata")
			continue
		}

		// Send to sync actor
		err := ctx.Tell(s.syncActor, BatchFetched{
			Data:     data,
			DAHeight: daHeight,
		})
		if err != nil {
			ctx.Logger().Error("failed to send data to sync", "error", err)
			break
		}

		s.dataRetrieved++
		ctx.Logger().Debug("data retrieved from p2p store",
			"height", data.Metadata.Height,
			"txCount", len(data.Txs))
	}

	s.lastDataStoreHeight = dataStoreHeight
}

// getHeadersFromStore retrieves headers from the header store
func (s *StoreSyncActor) getHeadersFromStore(ctx actor.Context, startHeight, endHeight uint64) ([]*types.SignedHeader, error) {
	if startHeight > endHeight {
		return nil, fmt.Errorf("invalid height range: %d > %d", startHeight, endHeight)
	}

	// Limit batch size to prevent OOM
	const maxBatchSize = 100
	if endHeight-startHeight+1 > maxBatchSize {
		endHeight = startHeight + maxBatchSize - 1
	}

	headers := make([]*types.SignedHeader, 0, endHeight-startHeight+1)

	for h := startHeight; h <= endHeight; h++ {
		header, err := s.headerStore.GetByHeight(context.Background(), h)
		if err != nil {
			return headers, fmt.Errorf("failed to get header at height %d: %w", h, err)
		}
		headers = append(headers, header)
	}

	return headers, nil
}

// getDataFromStore retrieves data from the data store
func (s *StoreSyncActor) getDataFromStore(ctx actor.Context, startHeight, endHeight uint64) ([]*types.Data, error) {
	if startHeight > endHeight {
		return nil, fmt.Errorf("invalid height range: %d > %d", startHeight, endHeight)
	}

	// Limit batch size to prevent OOM
	const maxBatchSize = 100
	if endHeight-startHeight+1 > maxBatchSize {
		endHeight = startHeight + maxBatchSize - 1
	}

	dataList := make([]*types.Data, 0, endHeight-startHeight+1)

	for h := startHeight; h <= endHeight; h++ {
		data, err := s.dataStore.GetByHeight(context.Background(), h)
		if err != nil {
			return dataList, fmt.Errorf("failed to get data at height %d: %w", h, err)
		}
		dataList = append(dataList, data)
	}

	return dataList, nil
}

// isExpectedProposer checks if header is from expected sequencer
func (s *StoreSyncActor) isExpectedProposer(header *types.SignedHeader) bool {
	return bytes.Equal(header.ProposerAddress, s.proposerAddress) &&
		header.ValidateBasic() == nil
}

// stopTimers stops all timers
func (s *StoreSyncActor) stopTimers() {
	if s.headerSyncTimer != nil {
		s.headerSyncTimer.Stop()
	}
	if s.dataSyncTimer != nil {
		s.dataSyncTimer.Stop()
	}
}

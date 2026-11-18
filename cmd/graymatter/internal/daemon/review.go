package daemon

import (
	"context"
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
	bolt "go.etcd.io/bbolt"
)

func (b rememberBackend) ReviewBackend() rpc.ReviewBackend {
	backend, _ := b.AdvancedStore.(rpc.ReviewBackend)
	return backend
}

// ReadStoreReviewOverview adds a side-effect-free projection to status.
func ReadStoreReviewOverview(db *bolt.DB, response *StoreOverviewResponse) error {
	overview, err := memory.ReadReviewOverview(context.Background(), db)
	if err != nil {
		return fmt.Errorf("review journal: %w", err)
	}
	response.Review = &overview
	return nil
}

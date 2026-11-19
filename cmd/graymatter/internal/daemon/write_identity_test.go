package daemon

import (
	"context"
	"errors"
	"strconv"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
	bolt "go.etcd.io/bbolt"
)

type rolledBackIdentityWriter struct {
	rpc.ConfidenceWriter
	db *bolt.DB
}

var errIdentityRollback = errors.New("injected write rollback")

func (b *rolledBackIdentityWriter) PutWithOptionsReturningFact(context.Context, string, string, memory.WriteOptions) (memory.Fact, error) {
	err := b.db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucket([]byte("identity-rollback-fixture")); err != nil {
			return err
		}
		return errIdentityRollback
	})
	return memory.Fact{}, err
}

func TestR4IdentityDaemonTriggersMaintenanceOnlyAfterCommit(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(strconv.FormatBool(async), func(t *testing.T) {
			cfg := graymatter.DefaultConfig()
			cfg.DataDir = t.TempDir()
			cfg.EmbeddingMode = graymatter.EmbeddingKeyword
			cfg.AsyncConsolidate = async
			cfg.AnthropicAPIKey, cfg.OpenAIAPIKey, cfg.VoyageAPIKey = "", "", ""
			mem, err := graymatter.NewWithConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mem.Close() })
			advanced := mem.Advanced()
			observed := &confidenceAsyncBackend{AdvancedStore: advanced, ConfidenceWriter: advanced.(rpc.ConfidenceWriter)}
			backend := rememberBackend{AdvancedStore: observed, mem: mem}
			ctx := context.Background()
			wantLaunches := 0
			if async {
				wantLaunches = 1
			}
			id, err := backend.PutWithOptionsReturningFact(ctx, "a", "confirmed maintenance input", memory.WriteOptions{})
			if err != nil || id.ID == "" || id.Confidence != "" || observed.launches != wantLaunches {
				t.Fatalf("confirmed write=%+v / %v, launches=%d want=%d", id, err, observed.launches, wantLaunches)
			}
			if async && (len(observed.observed) != 1 || observed.observed[0].ID != id.ID) {
				t.Fatalf("maintenance started before commit: %+v", observed.observed)
			}
			invalid := ""
			if id, err := backend.PutWithOptionsReturningFact(ctx, "a", "invalid options", memory.WriteOptions{Confidence: &invalid}); err == nil || id.ID != "" || observed.launches != wantLaunches {
				t.Fatalf("validation triggered maintenance: %+v / %v, launches=%d", id, err, observed.launches)
			}
			db := advanced.(interface{ DB() *bolt.DB }).DB()
			observed.ConfidenceWriter = &rolledBackIdentityWriter{ConfidenceWriter: observed.ConfidenceWriter, db: db}
			if id, err := backend.PutWithOptionsReturningFact(ctx, "a", "rolled back", memory.WriteOptions{}); !errors.Is(err, errIdentityRollback) || id.ID != "" || observed.launches != wantLaunches {
				t.Fatalf("rollback triggered maintenance: %+v / %v, launches=%d", id, err, observed.launches)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				if tx.Bucket([]byte("identity-rollback-fixture")) != nil {
					t.Error("rollback left a bucket")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			facts, err := advanced.List("a")
			if err != nil || len(facts) != 1 || facts[0].ID != id.ID {
				t.Fatalf("rejections changed the committed state: %+v / %v", facts, err)
			}
		})
	}
}

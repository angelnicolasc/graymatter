package memory

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestCitationBatchQuotaAccumulatesAllMembers(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir(), MaxQuoteBytes: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := revisionDBSnapshot(t, s)
	quote := strings.Repeat("q", 400_000)
	inputs := []WriteFact{
		{Text: "first assertion", Options: citationOptions("first", quote)},
		{Text: "second assertion", Options: citationOptions("second", quote)},
		{Text: "third assertion", Options: citationOptions("third", quote)},
	}
	got, err := s.PutManyReturningFacts(context.Background(), "a", inputs)
	if !errors.Is(err, ErrEvidenceQuota) || len(got) != 0 {
		t.Fatalf("batch exceeded shared transaction quota: %v, %v", got, err)
	}
	if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatal("quota rejection retained part of the facts, citations or roots")
	}
}

func TestCitationRevisionQuotaIncludesLineageAndQuote(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir(), MaxQuoteBytes: 700_000})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	// Each component fits alone. JSON escaping makes the source snapshot
	// roughly 540 KiB; its new citation must share the same transaction quota.
	source, err := s.PutReturningFact(ctx, "a", strings.Repeat("\x00", 90_000))
	if err != nil {
		t.Fatal(err)
	}
	before := revisionDBSnapshot(t, s)
	got, err := s.ReviseFactsWithOptions(ctx, "a", "replacement", citationOptions("doc", strings.Repeat("q", 550_000)), source)
	if !errors.Is(err, ErrEvidenceQuota) || got.ID != "" {
		t.Fatalf("revision reset its cumulative quota: %+v, %v", got, err)
	}
	if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatal("quota failure retired the source or retained a partial citation")
	}
}

func TestCitationPayloadCountIncludesBothNamespaces(t *testing.T) {
	s, err := Open(StoreConfig{DataDir: t.TempDir(), MaxQuoteBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, namespace := range []string{"a", "b"} {
		if _, err := s.PutWithOptionsReturningFact(ctx, namespace, "small assertion", citationOptions("doc", strings.Repeat(namespace, 8192))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		a, allA, err := evidencePayloadBytesTx(tx, "a")
		if err != nil {
			return err
		}
		b, allB, err := evidencePayloadBytesTx(tx, "b")
		if err != nil {
			return err
		}
		if a < 8192 || b < 8192 || allA != a+b || allB != allA {
			t.Fatalf("citation omitted from shared quota accounting: a=%d b=%d store=%d/%d", a, b, allA, allB)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

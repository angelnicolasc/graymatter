package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestRESTLineageProjectionAndDeleteRetention(t *testing.T) {
	ctx := context.Background()
	store, err := memory.Open(memory.StoreConfig{
		DataDir: t.TempDir(), Embedder: embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	original, err := store.PutReturningFact(ctx, "project", "The original shipment is amber")
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.ReviseFactsWithOptions(ctx, "project", "The revised shipment is green", memory.WriteOptions{}, original)
	if err != nil {
		t.Fatal(err)
	}
	sourceHash := memory.NewEvidenceSnapshot(original).ContentHash
	srv := New("", testStore{store}, nil, WithAuthToken(testToken))
	request := func(method, path string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer "+testToken)
		}
		srv.httpSrv.Handler.ServeHTTP(rec, req)
		return rec
	}

	facts := request(http.MethodGet, "/facts?agent=project", true)
	if facts.Code != http.StatusOK {
		t.Fatalf("facts: %d %s", facts.Code, facts.Body)
	}
	var listed map[string][]memory.Fact
	if err := json.Unmarshal(facts.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var active memory.Fact
	for _, f := range listed["facts"] {
		if f.ID == head.ID {
			active = f
		}
	}
	if len(listed) != 1 || active.LineageStatus != memory.LineageVerified ||
		len(active.Replaces) != 1 || active.Replaces[0].ContentHash != sourceHash {
		t.Fatalf("missing projected lineage: %s", facts.Body)
	}

	deleted := request(http.MethodDelete, "/forget/"+original.ID+"?agent=project", true)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body)
	}
	var receipt map[string]string
	if err := json.Unmarshal(deleted.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["deleted_id"] != original.ID || receipt["deletion_scope"] != "fact_record" ||
		!strings.Contains(receipt["lineage_retention"], "snapshots remain available") {
		t.Fatalf("wrong delete scope: %s", deleted.Body)
	}

	params := url.Values{"agent": {"project"}, "fact_id": {original.ID}, "content_hash": {sourceHash}}
	path := "/lineage?" + params.Encode()
	if rec := request(http.MethodGet, path, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous lineage access: %d %s", rec.Code, rec.Body)
	}
	evidence := request(http.MethodGet, path, true)
	if evidence.Code != http.StatusOK {
		t.Fatalf("lineage: %d %s", evidence.Code, evidence.Body)
	}
	var inspected memory.LineageInspection
	if err := json.Unmarshal(evidence.Body.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Snapshot == nil || inspected.Snapshot.Text != original.Text || inspected.ContentHash != sourceHash {
		t.Fatalf("exact source is unavailable: %s", evidence.Body)
	}
	params.Set("agent", "other")
	if rec := request(http.MethodGet, "/lineage?"+params.Encode(), true); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-namespace version lookup: %d %s", rec.Code, rec.Body)
	}
	recall := request(http.MethodGet, "/recall?agent=project&q=shipment", true)
	if recall.Code != http.StatusOK || strings.Contains(recall.Body.String(), original.Text) {
		t.Fatalf("lineage inspection changed recall eligibility: %d %s", recall.Code, recall.Body)
	}
}

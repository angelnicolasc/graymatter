package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	netrpc "net/rpc"
	"net/rpc/jsonrpc"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The old wire has no authoritative projection fields.
type legacyLineageFact struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"`
	Text        string    `json:"text"`
	Kind        string    `json:"kind,omitempty"`
	Confidence  string    `json:"confidence,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	AccessedAt  time.Time `json:"accessed_at"`
	AccessCount int       `json:"access_count"`
	Weight      float64   `json:"weight"`
	Embedding   []float32 `json:"embedding,omitempty"`
}

func TestLineageL06LegacyUpdatePreservesAuthority(t *testing.T) {
	store, _, client, _ := inspectionEmbedderServer(t)
	ctx := context.Background()
	source, err := store.PutReturningFact(ctx, "a", "original recovery policy")
	if err != nil {
		t.Fatal(err)
	}
	head, err := client.ReviseFactsWithOptions(ctx, "a", "revised recovery policy", memory.WriteOptions{}, source)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.InspectLineage(ctx, "a", head.ID, "")
	if err != nil || before.Snapshot == nil || len(before.Replaces) != 1 {
		t.Fatalf("missing authoritative lineage: %+v, %v", before, err)
	}

	req := struct {
		AgentID string
		Fact    legacyLineageFact
	}{"a", legacyLineageFact{
		ID: head.ID, AgentID: head.AgentID, Text: head.Text, Kind: head.Kind,
		Confidence: head.Confidence, CreatedAt: head.CreatedAt, AccessedAt: head.AccessedAt,
		AccessCount: head.AccessCount + 5, Weight: head.Weight, Embedding: head.Embedding,
	}}
	if err := client.CallService(ServiceName, "UpdateFact", &req, &struct{}{}, time.Second); err != nil {
		t.Fatalf("old client metadata update: %v", err)
	}
	facts, err := client.List("a")
	if err != nil || len(facts) != 2 {
		t.Fatalf("list after old client update: %+v, %v", facts, err)
	}
	current := facts[0]
	if current.ID != head.ID || current.AccessCount != req.Fact.AccessCount ||
		!reflect.DeepEqual(current.LineageProjection, before.LineageProjection) {
		t.Fatalf("old client update lost metadata or lineage: %+v", current)
	}

	req.Fact.Text = "legacy client attempts a content overwrite"
	err = client.CallService(ServiceName, "UpdateFact", &req, &struct{}{}, time.Second)
	if err == nil || !strings.Contains(err.Error(), memory.ErrRevisionRequired.Error()) {
		t.Fatalf("old client content overwrite error: %v", err)
	}
	changed := current
	changed.Text = req.Fact.Text
	if err := client.UpdateFact("a", changed); !errors.Is(err, memory.ErrRevisionRequired) {
		t.Fatalf("versioned UpdateFact lost typed rejection: %v", err)
	}
	afterFacts, err := client.List("a")
	if err != nil || !reflect.DeepEqual(facts, afterFacts) {
		t.Fatalf("rejected updates changed canonical rows: %+v, %v", afterFacts, err)
	}
	after, err := client.InspectLineage(ctx, "a", head.ID, before.ContentHash)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected updates changed lineage: %+v, %v", after, err)
	}
}

func TestLineageL17RPCReadProjectionsAndArchive(t *testing.T) {
	store, _, client, _ := inspectionEmbedderServer(t)
	ctx := context.Background()
	source, err := store.PutReturningFact(ctx, "a", "original deployment policy")
	if err != nil {
		t.Fatal(err)
	}
	head, err := client.CurateFact(ctx, "a", source.ID, "revise", "revised deployment policy", source)
	if err != nil || len(head.Replaces) != 1 {
		t.Fatalf("curation receipt lineage: %+v, %v", head, err)
	}
	proof := head.Replaces[0]
	if proof.FactID != source.ID || proof.ContentHash == "" {
		t.Fatalf("replacement does not name its source version: %+v", proof)
	}
	if err := store.Delete("a", source.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "other", "other namespace marker"); err != nil {
		t.Fatal(err)
	}

	facts, err := client.List("a")
	if err != nil || len(facts) != 1 || !reflect.DeepEqual(facts[0].LineageProjection, head.LineageProjection) {
		t.Fatalf("List projection: %+v, %v", facts, err)
	}
	page, err := client.ListFacts(ctx, "a", "all", "", "", 20)
	if err != nil || len(page.Facts) != 1 || !reflect.DeepEqual(page.Facts[0].LineageProjection, head.LineageProjection) {
		t.Fatalf("inspection page projection: %+v, %v", page, err)
	}
	receipts, err := client.RecallPreview(ctx, "a", "deployment policy", 5)
	if err != nil || len(receipts) != 1 || !reflect.DeepEqual(receipts[0].Provenance.LineageProjection, head.LineageProjection) {
		t.Fatalf("preview projection: %+v, %v", receipts, err)
	}

	retained, err := client.InspectLineage(ctx, "a", proof.FactID, proof.ContentHash)
	if err != nil || retained.Snapshot == nil || retained.Snapshot.Text != source.Text {
		t.Fatalf("deleted source version is not navigable: %+v, %v", retained, err)
	}
	if _, err := client.InspectLineage(ctx, "other", proof.FactID, proof.ContentHash); !errors.Is(err, memory.ErrFactNotFound) {
		t.Fatalf("cross-namespace lookup: %v", err)
	}

	for _, namespace := range []string{"a", ""} {
		direct, err := store.ExportLineage(ctx, namespace)
		if err != nil {
			t.Fatal(err)
		}
		remote, err := client.ExportLineage(ctx, namespace)
		if err != nil || !reflect.DeepEqual(direct, remote) {
			t.Fatalf("archive namespace %q: got %+v, want %+v, err %v", namespace, remote, direct, err)
		}
	}
}

type oversizedLineageArchiveBackend struct{ *memory.Store }

func (*oversizedLineageArchiveBackend) ExportLineage(context.Context, string) (memory.LineageArchive, error) {
	return memory.LineageArchive{Version: 1, Facts: []memory.Fact{
		{Text: "first row fits"},
		{Text: strings.Repeat("x", maxLineageArchiveBytes+1)},
	}}, nil
}

func TestLineageArchiveRPCRejectsOversizedSnapshotWithoutPartialRows(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client := confidenceClientForBackend(t, &oversizedLineageArchiveBackend{Store: store})
	archive, err := client.ExportLineage(context.Background(), "")
	if !errors.Is(err, ErrLineageArchiveTooLarge) || !reflect.DeepEqual(archive, memory.LineageArchive{}) {
		t.Fatalf("oversized archive returned partial data or hid reason: %+v, %v", archive, err)
	}
	if err := client.Ping(); err != nil {
		t.Fatalf("oversize rejection damaged connection: %v", err)
	}
}

func TestPU07LineageArchiveBoundsSubjectMetadata(t *testing.T) {
	ctx := context.Background()
	archive := memory.LineageArchive{Version: memory.LineageArchiveVersion, Facts: []memory.Fact{{Text: ""}}}
	initial, err := encodeLineageArchive(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	initialJSON, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	archive.Facts[0].Text = strings.Repeat("x", maxLineageArchiveBytes-len(initialJSON)-16)
	nearLimit, err := encodeLineageArchive(ctx, archive)
	if err != nil {
		t.Fatalf("archive below the limit was rejected: %v", err)
	}
	nearLimitJSON, err := json.Marshal(nearLimit)
	if err != nil || len(nearLimitJSON) != maxLineageArchiveBytes-16 {
		t.Fatalf("archive fixture size: %d bytes, %v", len(nearLimitJSON), err)
	}

	withHead := archive
	withHead.SubjectHeads = []memory.SubjectHeadInspection{{
		Namespace: "project", Name: "current release", State: "active",
		Reference: memory.LineageReference{FactID: "01ABCDEFGHIJKLMNOPQRSTUVWX", ContentHash: strings.Repeat("a", 64)},
	}}
	withIssue := archive
	withIssue.SubjectIssues = &memory.SubjectIssues{Total: 1, Subjects: []memory.SubjectIssue{{
		Namespace: "project", Name: "deployment policy", HeadFactID: "01ABCDEFGHIJKLMNOPQRSTUVWX",
	}}}
	for _, tc := range []struct {
		name    string
		archive memory.LineageArchive
	}{
		{"subject_heads", withHead},
		{"subject_issues", withIssue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeLineageArchive(context.Background(), tc.archive)
			if !errors.Is(err, ErrLineageArchiveTooLarge) || !reflect.DeepEqual(got, LineageArchiveResponse{}) {
				t.Fatalf("oversized metadata returned a partial archive or hid its reason: err=%v", err)
			}
		})
	}
}

type preSubjectArchiveService struct{ exportCalls atomic.Int32 }

func (*preSubjectArchiveService) Ping(_ *PingRequest, out *PingResponse) error {
	out.Protocol = Protocol
	out.Capabilities = []string{AuthoritativeLineageV1, InspectionContextV1}
	return nil
}

func (*preSubjectArchiveService) InspectLineage(_ *LineageInspectionRequest, out *LineageInspectionResponse) error {
	out.Lineage.ContentHash = "available"
	return nil
}

func (s *preSubjectArchiveService) ExportLineage(_ *LineageArchiveRequest, out *LineageArchiveResponse) error {
	s.exportCalls.Add(1)
	out.Version = memory.LineageArchiveVersion
	return nil
}

func TestPU07LineageArchiveRejectsPartialLegacyPeer(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	service := &preSubjectArchiveService{}
	server := netrpc.NewServer()
	if err := server.RegisterName(ServiceName, service); err != nil {
		t.Fatal(err)
	}
	go server.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	client := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: time.Second}
	t.Cleanup(func() { _ = client.Close(); _ = serverConn.Close() })

	ctx := context.Background()
	if lineage, err := client.InspectLineage(ctx, "project", "kept", ""); err != nil || lineage.ContentHash != "available" {
		t.Fatalf("old peer should still support inspection: %+v, %v", lineage, err)
	}
	archive, err := client.ExportLineage(ctx, "project")
	if !errors.Is(err, ErrLineageUnsupported) || !strings.Contains(err.Error(), "subject heads and diagnostics") ||
		!strings.Contains(err.Error(), "Update and restart") || !reflect.DeepEqual(archive, memory.LineageArchive{}) {
		t.Fatalf("old peer did not report an explicit rejection: archive=%+v err=%v", archive, err)
	}
	if service.exportCalls.Load() != 0 {
		t.Fatalf("export dispatched to old peer %d times", service.exportCalls.Load())
	}
}

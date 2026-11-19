package rpc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	bolt "go.etcd.io/bbolt"
)

func TestA108SubjectIssuesRPCRoundTrip(t *testing.T) {
	dir, server := startServer(t, "")
	client := dialT(t, dir)
	ctx := context.Background()
	name := "deployment window"
	rows, err := client.PutManyReturningReceipts(ctx, "project", []memory.WriteFact{
		{Text: "Deployment window was Tuesday", Options: memory.WriteOptions{Subject: &name}},
		{Text: "Deployment window is Thursday"},
	})
	if err != nil || len(rows) != 2 {
		t.Fatalf("seed facts: %+v %v", rows, err)
	}
	old, current := rows[0].Fact, rows[1].Fact
	// Simulate the published writer's tombstone without inventing an edge.
	if err := server.backend.(*memory.Store).DB().Update(func(tx *bolt.Tx) error {
		facts := tx.Bucket([]byte("facts")).Bucket([]byte("project"))
		var row map[string]json.RawMessage
		if err := json.Unmarshal(facts.Get([]byte(old.ID)), &row); err != nil {
			return err
		}
		row["superseded_by"], _ = json.Marshal(current.ID)
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		return facts.Put([]byte(old.ID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	got, err := client.SearchScoped(ctx, "project", []string{"deployment"}, 8, false, memory.RecallOptions{})
	if err != nil || len(got.Facts) != 1 || got.Facts[0].ID != current.ID || len(got.Facts[0].Subjects) != 0 {
		t.Fatalf("unexpected scoped result: %+v %v", got, err)
	}
	if got.SubjectIssues == nil || got.SubjectIssues.Total != 1 || len(got.SubjectIssues.Subjects) != 1 || got.SubjectIssues.Subjects[0].HeadFactID != old.ID {
		t.Fatalf("missing in-band subject issue: %+v", got.SubjectIssues)
	}
	issues, err := client.SubjectIssues(ctx, []string{"project"})
	if err != nil || issues.Total != 1 || len(issues.Subjects) != 1 || issues.Subjects[0].Name != name {
		t.Fatalf("unexpected diagnostic RPC: %+v %v", issues, err)
	}
	projects, err := client.SubjectIssuesByNamespace(ctx)
	if err != nil || len(projects) != 1 || projects["project"].Total != 1 {
		t.Fatalf("unexpected project diagnostics: %+v %v", projects, err)
	}
}

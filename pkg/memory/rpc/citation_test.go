package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	netrpc "net/rpc"
	"net/rpc/jsonrpc"
	"reflect"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestCitationRPCRoundTripAndHooks(t *testing.T) {
	dir, _ := startServer(t, "")
	c := dialT(t, dir)
	ctx := context.Background()
	source, quote := "dóc/原本:\"3\"\t", "Cité 中文\nLF\r\nCRLF\t\"quote\""
	facts, err := c.PutManyReturningFacts(ctx, "a", []memory.WriteFact{{Text: "citation deployment", Options: memory.WriteOptions{Source: &source, Quote: &quote}}})
	if err != nil || len(facts) != 1 {
		t.Fatalf("put %v %v", facts, err)
	}
	got, err := c.SearchScoped(ctx, "a", []string{"deployment"}, 8, false, memory.RecallOptions{})
	if err != nil || len(got.Facts) != 1 || got.Facts[0].Evidence == nil {
		t.Fatalf("search %+v %v", got, err)
	}
	f := got.Facts[0]
	if f.Evidence.Source != source || f.Evidence.Quote != quote {
		t.Fatal("search changed literal")
	}
	fetched, err := c.FetchCitation(ctx, "a", f.ID, f.ContentHash)
	if err != nil || fetched.Evidence == nil || fetched.Evidence.Source != source || fetched.Evidence.Quote != quote {
		t.Fatalf("fetch %+v %v", fetched, err)
	}
	hooks, err := c.RecallForHooks(ctx, "a", "deployment", 8)
	if err != nil || len(hooks) != 1 {
		t.Fatalf("hooks %+v %v", hooks, err)
	}
	rows, err := c.List("a")
	if err != nil || rows[0].Evidence == nil || rows[0].Evidence.Quote != quote {
		t.Fatalf("list %+v %v", rows, err)
	}
}

func TestCitationRPCRejectsU2BeforeMutation(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	clientConn, serverConn := net.Pipe()
	service := &lostUtilityReply{store: store, conn: serverConn}
	srv := netrpc.NewServer()
	if err := srv.RegisterName(ServiceName, service); err != nil {
		t.Fatal(err)
	}
	go srv.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	c := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: time.Second}
	t.Cleanup(func() { _ = c.Close(); _ = serverConn.Close() })
	quote := "must not be discarded"
	_, err = c.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "assertion", Options: memory.WriteOptions{Quote: &quote}}})
	if !errors.Is(err, memory.ErrCitationUnsupported) || service.calls.Load() != 0 {
		t.Fatalf("old daemon mutation err=%v calls=%d", err, service.calls.Load())
	}
	if _, err = c.FetchCitation(context.Background(), "a", "id", "hash"); !errors.Is(err, memory.ErrCitationUnsupported) {
		t.Fatalf("fetch old daemon: %v", err)
	}
	if _, err = c.SearchScoped(context.Background(), "a", []string{"query"}, 8, false, memory.RecallOptions{}); !errors.Is(err, memory.ErrCitationUnsupported) {
		t.Fatalf("search old daemon: %v", err)
	}
}

func TestCitationRPCNullOptionsRejected(t *testing.T) {
	for _, field := range []string{"source", "quote"} {
		body := []byte(`{"Options":{"` + field + `":null}}`)
		for _, dst := range []any{&PutWithOptionsRequest{}, &ReviseWithOptionsRequest{}, &ReviseFactsWithOptionsRequest{}} {
			if err := json.Unmarshal(body, dst); err == nil {
				t.Fatalf("null %s accepted by %T", field, dst)
			}
		}
		var batch PutManyRequest
		if err := json.Unmarshal([]byte(`{"Facts":[{"text":"x","options":{"`+field+`":null}}]}`), &batch); err == nil {
			t.Fatalf("null batch %s accepted", field)
		}
	}
}

func TestCitationRPCHooksPreserveLegacyWeightAndEmptyTopK(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), ConfidenceWeight: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	verified, unverified := "verified", "unverified"
	for _, input := range []memory.WriteFact{{Text: "deployment older verified", Options: memory.WriteOptions{Confidence: &verified}}, {Text: "deployment recent unverified", Options: memory.WriteOptions{Confidence: &unverified}}} {
		if _, err := store.PutWithOptionsReturningFact(context.Background(), "a", input.Text, input.Options); err != nil {
			t.Fatal(err)
		}
	}
	srv := NewServer(store, nil)
	for _, topK := range []int{8, 0, -1} {
		var legacy RecallResponse
		if err := srv.Recall(&RecallRequest{AgentID: "a", Query: "deployment", TopK: topK}, &legacy); err != nil {
			t.Fatal(err)
		}
		var hooks RecallForHooksResponse
		if err := srv.RecallForHooks(&RecallForHooksRequest{AgentID: "a", Query: "deployment", TopK: topK}, &hooks); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(legacy.Facts, hooks.Facts) {
			t.Fatalf("top_k=%d legacy=%v hooks=%v", topK, legacy.Facts, hooks.Facts)
		}
	}
}

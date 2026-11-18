package daemon

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func TestConnectContextCanceledDuringPingDoesNotRespawn(t *testing.T) {
	dir := t.TempDir()
	token := "synthetic-consolidation-ping-token"
	ln, cleanup, err := rpc.Listen(dir, token)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer ln.Close()
	pinging := make(chan error, 1)
	connectionClosed := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			pinging <- err
			return
		}
		defer c.Close()
		if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			pinging <- err
			return
		}
		r := bufio.NewReader(c)
		auth, err := r.ReadString('\n')
		if err != nil || strings.TrimSpace(auth) != token {
			pinging <- errors.New("fixture did not receive the expected token")
			return
		}
		request, err := r.ReadString('\n')
		if err != nil || !strings.Contains(request, "GrayMatter.Ping") {
			pinging <- errors.New("fixture did not receive the readiness ping")
			return
		}
		pinging <- nil
		_, err = io.Copy(io.Discard, r)
		connectionClosed <- err
	}()

	oldResolve := resolveExecutable
	resolveExecutable = func() (string, error) {
		t.Error("canceled readiness triggered a daemon spawn")
		return "", errors.New("unexpected daemon spawn")
	}
	t.Cleanup(func() { resolveExecutable = oldResolve })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		c, err := ConnectContext(ctx, dir)
		if c != nil {
			_ = c.Close()
		}
		result <- err
	}()
	select {
	case err := <-pinging:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("fixture did not observe the ping")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readiness cancellation = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ConnectContext did not return after cancellation")
	}
	select {
	case err := <-connectionClosed:
		if err != nil {
			t.Fatalf("dedicated client connection did not close cleanly: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dedicated client connection remained open")
	}
}

type reportingBackendFixture struct {
	graymatter.AdvancedStore
	supported bool
	received  string
}

func (b *reportingBackendFixture) SupportsConsolidationReport() bool { return b.supported }

func (b *reportingBackendFixture) ConsolidateWithReport(_ context.Context, agent string, _ memory.ConsolidateConfig) (memory.ConsolidationReport, error) {
	b.received = agent
	return memory.ConsolidationReport{Status: memory.ConsolidationSkipped, Reason: "no_facts"}, nil
}

func TestRememberBackendReportingCapabilityIsConditional(t *testing.T) {
	legacy := struct{ graymatter.AdvancedStore }{}
	for _, store := range []graymatter.AdvancedStore{legacy, &reportingBackendFixture{}, (*reportingBackendFixture)(nil)} {
		if b := withConsolidationReporting(rememberBackend{AdvancedStore: store}); rpc.SupportsConsolidationReporting(b) {
			t.Fatalf("unavailable backend %T acquired report support", store)
		}
	}
	store := &reportingBackendFixture{supported: true}
	b := withConsolidationReporting(rememberBackend{AdvancedStore: store})
	if !rpc.SupportsConsolidationReporting(b) {
		t.Fatal("reporting capability lost in daemon wrapper")
	}
	report, err := b.(memory.ReportingConsolidator).ConsolidateWithReport(context.Background(), "audit", nil)
	if err != nil || report.Status != memory.ConsolidationSkipped || store.received != "audit" {
		t.Fatalf("daemon did not forward report to real store: %+v, %v", report, err)
	}
	store.supported = false
	if rpc.SupportsConsolidationReporting(b) {
		t.Fatal("wrapper continued to claim support after its store declined it")
	}
}

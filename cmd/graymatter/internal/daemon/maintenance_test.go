package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

// A legacy peer still exposes status and shutdown, but returns no policy.
type legacyMaintenanceHost struct{ *Host }

func (h legacyMaintenanceHost) StoreOverview(req *StoreOverviewRequest, resp *StoreOverviewResponse) error {
	err := h.Host.StoreOverview(req, resp)
	resp.MaintenanceMode = ""
	return err
}

func TestMaintenancePolicyRoundTrip(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "VOYAGE_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://maintenance-policy")

	for _, tc := range []struct {
		name       string
		legacy     bool
		pingOnDial bool
	}{
		{name: "legacy", legacy: true, pingOnDial: true},
		{name: "current", pingOnDial: true},
		{name: "current-lazy-handshake"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newDirectHost(t)
			first, err := h.mem.RememberWithOptions(ctx, "agent", "original alpha", memory.WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			second, err := h.mem.RememberWithOptions(ctx, "agent", "original beta", memory.WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			backend := rememberBackend{AdvancedStore: h.mem.Advanced(), mem: h.mem}
			var service rpc.Backend = backend
			var host any = h
			if tc.legacy {
				// The interface hides the additional Ping capabilities of the
				// backend while retaining the methods of a legacy peer.
				service = struct{ rpc.Backend }{backend}
				host = legacyMaintenanceHost{h}
			}
			srv := rpc.NewServer(service, h.mem.Config())
			srv.RegisterExtra(HostServiceName, host)
			dir := t.TempDir()
			ln, cleanup, err := rpc.Listen(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(srv.Stop)
			conn, err := rpc.Dial(rpc.DialOptions{DataDir: dir, PingOnDial: tc.pingOnDial})
			if err != nil {
				t.Fatal(err)
			}
			c := &Client{Client: conn}
			t.Cleanup(func() { _ = c.Close() })

			wantMode := MaintenanceOnly
			if tc.legacy {
				wantMode = ""
			}
			overview, err := c.StoreOverview()
			if err != nil || overview.MaintenanceMode != wantMode || overview.TotalLiveFacts != 2 {
				t.Fatalf("read-only overview = %+v, %v; want mode %q and two live facts", overview, err, wantMode)
			}
			if !tc.pingOnDial && c.MaintenanceMode() != "" {
				t.Fatal("unnegotiated capability was presented as verified")
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if err := c.Consolidate(canceled, "agent"); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled maintenance returned %v", err)
			}
			operations := []struct {
				name string
				run  func() error
			}{
				{"consolidate", func() error { return c.Consolidate(ctx, "agent") }},
				{"remember", func() error { return c.Remember(ctx, "agent", "ordinary fact") }},
				{"put", func() error { return c.Put(ctx, "agent", "direct put") }},
				{"put returning fact", func() error {
					_, err := c.PutReturningFact(ctx, "agent", "returning fact")
					return err
				}},
				{"put with options", func() error {
					_, err := c.PutWithOptionsReturningFact(ctx, "agent", "options fact", memory.WriteOptions{})
					return err
				}},
				{"revise", func() error {
					_, err := c.ReviseWithOptions(ctx, "agent", first.Text, "revised alpha", memory.WriteOptions{})
					return err
				}},
				{"revise facts", func() error {
					_, err := c.ReviseFactsWithOptions(ctx, "agent", "revised beta", memory.WriteOptions{}, second)
					return err
				}},
			}
			for _, op := range operations {
				err := op.run()
				if tc.legacy && !errors.Is(err, ErrMaintenancePolicyUnsupported) || !tc.legacy && err != nil {
					t.Errorf("%s returned %v", op.name, err)
				}
			}
			if mode := c.MaintenanceMode(); mode != wantMode {
				t.Errorf("cached policy = %q; want %q", mode, wantMode)
			}

			facts, err := c.List("agent")
			if err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				if len(facts) != 2 || facts[0].IsSuperseded() || facts[1].IsSuperseded() {
					t.Fatalf("legacy preflight changed the originals: %+v", facts)
				}
			} else {
				revisions := 0
				for _, fact := range facts {
					if fact.IsSuperseded() {
						revisions++
					}
				}
				if len(facts) != 8 || revisions != 2 {
					t.Fatalf("maintenance guard changed explicit writes or revisions: %+v", facts)
				}
			}
			// A shared write does not schedule maintenance on either generation.
			if err := c.PutShared(ctx, "shared fact"); err != nil {
				t.Fatalf("shared write: %v", err)
			}
			shared, err := c.List(memory.SharedAgentID)
			if err != nil || len(shared) != 1 {
				t.Fatalf("shared = %+v, %v", shared, err)
			}
			if err := c.Shutdown(); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
		})
	}
}

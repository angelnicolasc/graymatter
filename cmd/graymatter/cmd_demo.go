package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
)

// demoCmd is the "show me" command: one invocation plants a multi-agent
// corpus, runs consolidation, turns on the knowledge graph, and opens the TUI
// on the demo store. No API keys, no Ollama — the keyword embedder is the
// default when nothing else is configured, so the whole path is offline.
func demoCmd() *cobra.Command {
	var (
		useTmp  bool
		script  bool
		noTUI   bool
		fresh   bool
		seedDir string
	)
	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Seed a demo store and open it in the TUI — 30 seconds, one command",
		Long: `Creates a demo store with a realistic multi-agent corpus (a sales
closer, a support lead, and an infra agent), runs two consolidation
cycles, enables knowledge-graph auto-population, and opens the TUI.

Everything is local: no API keys, no Ollama, no network. After the TUI,
try the rest:

  graymatter kg render --out kg-graph.html   # the graph, as a self-contained page
  graymatter recall sales-closer "Maria"     # receipts-style retrieval
  graymatter export --format obsidian --out demo-vault --include-graph

This is a scratch store. Your own agents start with: graymatter init`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := seedDir
			if useTmp {
				tmp, err := os.MkdirTemp("", "graymatter-demo-")
				if err != nil {
					return fmt.Errorf("create temp dir: %w", err)
				}
				dir = tmp
			}
			if dir == "" {
				dir = defaultDemoDir
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return fmt.Errorf("resolve demo dir: %w", err)
			}

			if script {
				return printDemoScript(cmd, abs)
			}

			if fresh {
				if err := removeDemoDir(abs); err != nil {
					return err
				}
			}

			// The KG sentinel must exist before the daemon spawns: a daemon
			// that started earlier keeps its own wiring.
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return fmt.Errorf("create demo dir: %w", err)
			}
			if err := os.WriteFile(daemon.KGSentinelPath(abs), nil, 0o644); err != nil {
				return fmt.Errorf("write kg sentinel: %w", err)
			}

			store, err := openStoreIn(abs)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			planted, err := seedDemoCorpus(store)
			if err != nil {
				return err
			}

			// Two consolidation cycles per agent. With no LLM configured this
			// applies decay and pruning deterministically; with one configured
			// it also summarises — the demo does not depend on either.
			ctx := context.Background()
			for _, agent := range demoAgents() {
				for cycle := 0; cycle < 2; cycle++ {
					if err := store.Consolidate(ctx, agent.id); err != nil {
						return fmt.Errorf("consolidate %s: %w", agent.id, err)
					}
				}
			}

			if !quiet {
				fmt.Printf("GrayMatter demo ready at %s\n", abs)
				fmt.Printf("  %d agent(s), %d fact(s) planted, 2 consolidation cycles, knowledge graph on\n\n", len(demoAgents()), planted)
				fmt.Println("The TUI is opening. After it, try:")
				fmt.Println("  graymatter --dir \"" + abs + "\" kg render --out kg-graph.html")
				fmt.Println("  graymatter --dir \"" + abs + "\" recall sales-closer \"Maria follow up\"")
				fmt.Println("  graymatter --dir \"" + abs + "\" export --format obsidian --out demo-vault --include-graph")
				fmt.Println("\nThis is a scratch store. For your own agents: graymatter init")
			}

			if noTUI {
				return nil
			}

			// Open the TUI on the demo store: same entry point the tui command
			// uses, with the data dir pointed at the demo.
			dataDir = abs
			return tuiCmd().RunE(cmd, nil)
		},
	}
	cmd.Flags().BoolVar(&useTmp, "tmp", false, "seed the demo in a fresh temp directory instead of .graymatter-demo")
	cmd.Flags().BoolVar(&script, "script", false, "print the equivalent shell script instead of running it — no magic")
	cmd.Flags().BoolVar(&noTUI, "no-tui", false, "seed the store and print instructions without opening the TUI")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "delete the demo directory first and rebuild from scratch")
	cmd.Flags().StringVar(&seedDir, "dir", "", "seed the demo in this specific directory")
	_ = cmd.Flags().MarkHidden("dir")
	return cmd
}

const defaultDemoDir = ".graymatter-demo"

// openStoreIn opens the store rooted at dir, daemon mode included — the demo
// runs exactly the machinery production runs, so what the TUI shows is what
// an agent would see.
func openStoreIn(dir string) (cliStore, error) {
	prev := dataDir
	dataDir = dir
	defer func() { dataDir = prev }()
	return openStore()
}

type demoAgent struct {
	id    string
	facts []string
}

// demoAgents is the planted corpus: three agents with an interlocking
// story (a deal, its support tickets, the infra behind both) plus the
// project-wide shared namespace. The overlap is deliberate — consolidation
// and the KG have something to chew on.
func demoAgents() []demoAgent {
	return []demoAgent{
		{id: "sales-closer", facts: []string{
			"Maria at Acme Corp did not reply to the Wednesday follow-up; the third touchpoint is due Friday.",
			"Acme Corp's contract renewal is worth $84k ARR and renews on October 14th.",
			"Maria replies within a day to short emails with one clear ask; long summaries get ignored.",
			"The Acme pilot covers 40 seats and they asked about SSO before expanding.",
			"Acme's security review requires SOC 2 evidence; the report was sent on 2026-08-20.",
			"Acme's CFO wants annual billing; Maria's team pushed back for monthly.",
		}},
		{id: "support-lead", facts: []string{
			"Acme Corp opened ticket 4417: SAML login loop on Safari; clearing cookies is the workaround.",
			"Ticket 4417 was escalated to engineering with a fix ETA of next sprint.",
			"Two tickets this month mention slow search past 10k stored facts.",
			"The support rotation covers APAC on Mondays and Wednesdays.",
		}},
		{id: "infra-bot", facts: []string{
			"The production deploy window is Tuesday 09:00 UTC and rollbacks take under 5 minutes.",
			"Postgres connection pool saturated during the 09:00 backup; max_conns was raised to 200.",
			"Ollama runs on the GPU box at 10.0.3.21 serving llama3.2 for consolidation.",
			"Staging restarts every night at 02:00 UTC.",
		}},
		{id: "__shared__", facts: []string{
			"Project convention: every timestamp is stored as UTC ISO-8601.",
			"Standup is async in the team channel; blockers are posted by 10:00 local time.",
		}},
	}
}

// seedDemoCorpus plants the corpus into an empty store and is a no-op for
// agents that already have facts (the store is append-only, so re-running on
// a seeded demo must not duplicate everything).
func seedDemoCorpus(store cliStore) (int, error) {
	existing, err := store.ListAgents()
	if err != nil {
		return 0, fmt.Errorf("list agents: %w", err)
	}
	hasFacts := make(map[string]bool, len(existing))
	for _, a := range existing {
		fs, err := store.List(a)
		if err != nil {
			return 0, fmt.Errorf("list %s: %w", a, err)
		}
		if len(fs) > 0 {
			hasFacts[a] = true
		}
	}

	ctx := context.Background()
	planted := 0
	for _, agent := range demoAgents() {
		if hasFacts[agent.id] {
			continue
		}
		for _, fact := range agent.facts {
			if err := store.Remember(ctx, agent.id, fact); err != nil {
				return planted, fmt.Errorf("remember for %s: %w", agent.id, err)
			}
			planted++
		}
	}
	return planted, nil
}

// removeDemoDir deletes a previous demo directory, refusing anything that
// does not look like a GrayMatter data dir (an empty dir, a MEMORY.md, or a
// gray.db — nothing else).
func removeDemoDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "gray.db", "MEMORY.md", "gray.db.lock", "hooks.log", "kg.auto", "daemon.log", "vectors", "hooks", "export":
		default:
			return fmt.Errorf("refusing to delete %s: it contains %q, which is not a GrayMatter data file", dir, e.Name())
		}
	}
	return os.RemoveAll(dir)
}

// printDemoScript emits the equivalent shell steps — the point of --script is
// that a skeptical reader can see there is no hidden state.
func printDemoScript(cmd *cobra.Command, abs string) error {
	var sb strings.Builder
	sb.WriteString("#!/usr/bin/env sh\n")
	sb.WriteString("# graymatter demo --script — the exact steps `graymatter demo` runs.\n")
	sb.WriteString("# Generated by: graymatter demo --script\n\n")
	sb.WriteString(fmt.Sprintf("mkdir -p %q\n", abs))
	sb.WriteString(fmt.Sprintf("touch %q\n", daemon.KGSentinelPath(abs)))
	sb.WriteString("\n# plant the corpus (each fact a durable sentence):\n")
	for _, agent := range demoAgents() {
		for _, fact := range agent.facts {
			sb.WriteString(fmt.Sprintf("graymatter --dir %q remember %q %q\n", abs, agent.id, fact))
		}
	}
	sb.WriteString("\n# two consolidation cycles per agent:\n")
	for _, agent := range demoAgents() {
		sb.WriteString(fmt.Sprintf("graymatter --dir %q consolidate %q\n", abs, agent.id))
		sb.WriteString(fmt.Sprintf("graymatter --dir %q consolidate %q\n", abs, agent.id))
	}
	sb.WriteString("\n# render the graph and open the dashboard:\n")
	sb.WriteString(fmt.Sprintf("graymatter --dir %q kg render --out kg-graph.html\n", abs))
	sb.WriteString(fmt.Sprintf("graymatter --dir %q tui\n", abs))
	_, err := fmt.Fprint(cmd.OutOrStdout(), sb.String())
	return err
}

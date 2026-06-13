package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
)

func runCmd() *cobra.Command {
	var (
		background     bool
		resumeID       string
		maxRetries     int
		inputs         []string
		backgroundChild bool
		sessionIDFlag  string
	)

	cmd := &cobra.Command{
		Use:   "run <agent.md>",
		Short: "Run an agent defined by a SKILL.md-format file",
		Long: `Run executes an agent described by an agent Markdown file.

The agent file must follow the SKILL.md format (YAML frontmatter + Markdown sections).
Memory is automatically recalled before the run and stored after each response.

Examples:
  graymatter run sales-closer.md
  graymatter run sales-closer.md --input task="follow up Maria"
  graymatter run sales-closer.md --background
  graymatter run sales-closer.md --resume latest`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentFile := args[0]

			// Parse --input key=value pairs.
			inputMap := parseInputFlags(inputs)

			// --- Background parent: spawn child and exit ---
			if background && !backgroundChild {
				return spawnBackground(agentFile, dataDir, sessionIDFlag, os.Args[1:])
			}

			// --- Foreground or background child: run the agent ---
			// Route persistence through the shared store (daemon by default)
			// so a run never fights the bbolt lock with a TUI or MCP server.
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			cfg := harness.RunConfig{
				AgentFile:  agentFile,
				Inputs:     inputMap,
				DataDir:    dataDir,
				MaxRetries: maxRetries,
				ResumeID:   resumeID,
				Stdout:     cmd.OutOrStdout(),
				Stderr:     cmd.ErrOrStderr(),
				Store:      store,
			}
			result, err := harness.Run(context.Background(), cfg)
			if err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.ErrOrStderr(), "\n[Session %s | %d attempt(s)]\n", result.SessionID, result.Attempts)
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&background, "background", "b", false, "run agent in background (detach from terminal)")
	cmd.Flags().StringVarP(&resumeID, "resume", "r", "", `resume from a session ID or "latest"`)
	cmd.Flags().IntVar(&maxRetries, "max-retries", 3, "maximum number of LLM call attempts")
	cmd.Flags().StringArrayVarP(&inputs, "input", "i", nil, "input variable in key=value format (repeatable)")

	// Hidden internal flags used by the parent→child background spawn protocol.
	cmd.Flags().BoolVar(&backgroundChild, "background-child", false, "")
	cmd.Flags().StringVar(&sessionIDFlag, "session-id", "", "")
	_ = cmd.Flags().MarkHidden("background-child")
	_ = cmd.Flags().MarkHidden("session-id")

	return cmd
}

// spawnBackground re-invokes the current binary as a detached child process.
// The child receives --background-child and --session-id=<sid> flags so it
// knows to run as the background worker. The parent writes a PID file and exits.
func spawnBackground(agentFile, dir, providedSID string, origArgs []string) error {
	sessionID := providedSID
	if sessionID == "" {
		sessionID = ulid.Make().String()
	}

	// Ensure runtime directories exist.
	runDir := filepath.Join(dir, "run")
	logsDir := filepath.Join(dir, "logs")
	for _, d := range []string{runDir, logsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	logFile := filepath.Join(logsDir, sessionID+".log")
	pidFile := filepath.Join(runDir, sessionID+".pid")

	// Resolve the path to the current executable.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	// Build child args: remove --background/-b, add --background-child and --session-id.
	childArgs := buildChildArgs(origArgs, sessionID)

	// Open log file for child stdout+stderr.
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	c := exec.Command(exe, childArgs...)
	c.SysProcAttr = detachSysProcAttr()
	c.Stdout = lf
	c.Stderr = lf
	c.Stdin = nil

	if err := c.Start(); err != nil {
		_ = lf.Close()
		return fmt.Errorf("spawn background process: %w", err)
	}
	_ = lf.Close()

	pid := c.Process.Pid

	// Write PID file so "graymatter sessions kill" can terminate the process.
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		// Non-fatal: session kill will fail gracefully later.
		fmt.Fprintf(os.Stderr, "warn: write pid file: %v\n", err)
	}

	fmt.Printf("Session %s started in background.\nPID: %d\nLog: %s\n", sessionID, pid, logFile)
	return nil
}

// buildChildArgs strips --background/-b from origArgs and appends
// --background-child and --session-id=<sid>.
func buildChildArgs(origArgs []string, sessionID string) []string {
	out := make([]string, 0, len(origArgs)+2)
	for _, a := range origArgs {
		if a == "--background" || a == "-b" {
			continue
		}
		out = append(out, a)
	}
	out = append(out, "--background-child", "--session-id="+sessionID)
	return out
}

// parseInputFlags converts []string{"key=value", ...} to map[string]string.
func parseInputFlags(inputs []string) map[string]string {
	if len(inputs) == 0 {
		return nil
	}
	m := make(map[string]string, len(inputs))
	for _, kv := range inputs {
		k, v, _ := strings.Cut(kv, "=")
		if k != "" {
			m[strings.TrimSpace(k)] = v
		}
	}
	return m
}

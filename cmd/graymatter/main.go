package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// devVersion is what a build reports when it has no better answer.
const devVersion = "dev"

// version is injected at build time via -ldflags="-X main.version=x.y.z".
// GoReleaser does that; `go install` and a plain `go build` do not, which is
// why every binary anyone installs used to call itself "dev". resolveVersion
// recovers the module version Go embeds in every binary, so main() can give
// this variable a real answer before any command reads it.
var version = devVersion

// pickVersion resolves the version to report. An injected value always wins:
// it is the release number, and it is the only source that knows the tag.
// Otherwise the module version Go recorded at build time is used — a real
// semver for `go install module@vX.Y.Z`, a pseudo-version for a branch build,
// and "(devel)" for a build with no module info, which is no better than dev.
//
// Split out from main so it can be tested without faking a build.
func pickVersion(injected, module string) string {
	if injected != "" && injected != devVersion {
		return injected
	}
	switch module {
	case "", devVersion, "(devel)":
		return devVersion
	}
	return module
}

// resolveVersion applies pickVersion to this binary's own build info.
func resolveVersion() string {
	module := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		module = info.Main.Version
	}
	return pickVersion(version, module)
}

var (
	dataDir  string
	quiet    bool
	jsonOut  bool
	noDaemon bool
)

var rootCmd = &cobra.Command{
	Use:     "graymatter",
	Short:   "Persistent memory for AI agents",
	Long:    "GrayMatter gives AI agents persistent memory across runs.\nSingle binary. Zero infra. Works with Claude Code or any Go CLI agent.",
	Version: version,
}

func main() {
	// Resolve before any command runs: the TUI header, `--version` and the
	// MCP handshake all read this variable, and they must agree.
	version = resolveVersion()
	rootCmd.Version = version

	rootCmd.PersistentFlags().StringVar(&dataDir, "dir", ".graymatter", "data directory")
	rootCmd.PersistentFlags().BoolVar(&quiet, "quiet", false, "suppress non-essential output")
	rootCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "output in JSON format")
	rootCmd.PersistentFlags().BoolVar(&noDaemon, "no-daemon", false,
		"open the store in-process instead of through the daemon (debugging; fights the single-writer lock)")

	rootCmd.AddCommand(
		initCmd(),
		doctorCmd(),
		daemonCmd(),
		rememberCmd(),
		recallCmd(),
		checkpointCmd(),
		benchCmd(),
		mcpCmd(),
		exportCmd(),
		tuiCmd(),
		runCmd(),
		sessionsCmd(),
		pluginCmd(),
		serverCmd(),
		contextSyncCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

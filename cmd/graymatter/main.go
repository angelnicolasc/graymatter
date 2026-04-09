package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is injected at build time via -ldflags="-X main.version=x.y.z"
var version = "dev"

var (
	dataDir string
	quiet   bool
	jsonOut bool
)

var rootCmd = &cobra.Command{
	Use:     "graymatter",
	Short:   "Persistent memory for AI agents",
	Long:    "GrayMatter gives AI agents persistent memory across runs.\nSingle binary. Zero infra. Works with Claude Code or any Go CLI agent.",
	Version: version,
}

func main() {
	rootCmd.PersistentFlags().StringVar(&dataDir, "dir", ".graymatter", "data directory")
	rootCmd.PersistentFlags().BoolVar(&quiet, "quiet", false, "suppress non-essential output")
	rootCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "output in JSON format")

	rootCmd.AddCommand(
		initCmd(),
		rememberCmd(),
		recallCmd(),
		checkpointCmd(),
		mcpCmd(),
		exportCmd(),
		tuiCmd(),
		runCmd(),
		sessionsCmd(),
		pluginCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

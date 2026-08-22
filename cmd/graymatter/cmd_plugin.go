package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/plugin"
)

func pluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage GrayMatter plugins",
		Long:  "Install, list, and remove GrayMatter plugins.\nPlugins are Go binaries that extend the MCP tool surface via a JSON line protocol.",
	}
	cmd.AddCommand(
		pluginInstallCmd(),
		pluginListCmd(),
		pluginRemoveCmd(),
	)
	return cmd
}

func pluginInstallCmd() *cobra.Command {
	var (
		assumeYes bool
		insecure  bool
	)

	cmd := &cobra.Command{
		Use:   "install <manifest-url-or-path>",
		Short: "Install a plugin from a manifest file or HTTPS URL",
		Long: `Install a plugin from a manifest file or HTTPS URL.

Installing a plugin grants code execution: the binary it names runs on this
machine whenever an agent calls one of its tools. So the manifest must carry a
"sha256" digest of that binary, the digest is verified before anything is
written, and the binary is copied into <data-dir>/plugins/<name>/bin/ so what
runs later is the reviewed bytes rather than whatever sits at an external path
by then.

Manifests are fetched over HTTPS only. --insecure allows plaintext http for
testing against a local server.

Compute the digest with:

  sha256sum ./my-plugin            # Linux / macOS
  Get-FileHash ./my-plugin.exe     # Windows PowerShell`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := []plugin.InstallOption{plugin.WithConfirm(confirmPluginInstall(cmd, assumeYes))}
			if insecure {
				opts = append(opts, plugin.WithInsecureHTTP())
			}
			if err := plugin.Install(args[0], pluginsDir(), opts...); err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Plugin installed from %q.\n", args[0])
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "install without asking for confirmation")
	cmd.Flags().BoolVar(&insecure, "insecure", false,
		"allow fetching the manifest over plaintext http (development only)")
	return cmd
}

// errInstallDeclined is returned when the user answers no at the prompt. It is
// a normal outcome, so it reads like one.
var errInstallDeclined = errors.New("plugin install: cancelled")

// confirmPluginInstall builds the reviewer Install calls once the manifest is
// verified and before anything is written. Answering anything but yes aborts.
func confirmPluginInstall(cmd *cobra.Command, assumeYes bool) func(plugin.PluginManifest) error {
	return func(m plugin.PluginManifest) error {
		out := cmd.OutOrStdout()

		tools := make([]string, 0, len(m.Tools))
		for _, t := range m.Tools {
			tools = append(tools, t.Name)
		}
		toolList := strings.Join(tools, ", ")
		if toolList == "" {
			toolList = "(none)"
		}

		if assumeYes {
			if !quiet {
				fmt.Fprintf(out, "Installing plugin %q %s (sha256 %s), tools: %s\n",
					m.Name, m.Version, m.SHA256, toolList)
			}
			return nil
		}

		fmt.Fprintf(out, "About to install a plugin. It can run code on this machine.\n\n")
		fmt.Fprintf(out, "  name        %s\n", m.Name)
		fmt.Fprintf(out, "  version     %s\n", m.Version)
		if m.Description != "" {
			fmt.Fprintf(out, "  description %s\n", m.Description)
		}
		fmt.Fprintf(out, "  tools       %s\n", toolList)
		fmt.Fprintf(out, "  sha256      %s\n", m.SHA256)
		fmt.Fprintf(out, "  installs to %s\n\n", filepath.Dir(m.Binary))
		fmt.Fprint(out, "Install it? [y/N] ")

		reader := bufio.NewReader(cmd.InOrStdin())
		answer, err := reader.ReadString('\n')
		if err != nil && answer == "" {
			// No terminal to ask on. Refuse rather than assume consent;
			// scripts should pass --yes.
			fmt.Fprintln(out)
			return fmt.Errorf("%w: no answer on stdin (pass --yes to install non-interactively)", errInstallDeclined)
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return nil
		default:
			return errInstallDeclined
		}
	}
}

func pluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed plugins",
		RunE: func(cmd *cobra.Command, args []string) error {
			plugins, err := plugin.List(pluginsDir())
			if err != nil {
				return err
			}

			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(plugins)
			}

			if len(plugins) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No plugins installed.")
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVERSION\tDESCRIPTION\tTOOLS")
			for _, p := range plugins {
				tools := ""
				for i, t := range p.Tools {
					if i > 0 {
						tools += ", "
					}
					tools += t.Name
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.Version, p.Description, tools)
			}
			return w.Flush()
		},
	}
}

func pluginRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := plugin.Remove(args[0], pluginsDir()); err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Plugin %q removed.\n", args[0])
			}
			return nil
		},
	}
}

// pluginsDir returns <dataDir>/plugins.
func pluginsDir() string {
	return dataDir + "/plugins"
}

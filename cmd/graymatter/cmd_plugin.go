package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/pkg/plugin"
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
	return &cobra.Command{
		Use:   "install <manifest-url-or-path>",
		Short: "Install a plugin from a manifest file or HTTPS URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pluginDir := pluginsDir()
			if err := plugin.Install(args[0], pluginDir); err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Plugin installed from %q.\n", args[0])
			}
			return nil
		},
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

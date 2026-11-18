package main

import (
	"fmt"
	gmcp "github.com/angelnicolasc/graymatter/cmd/graymatter/internal/mcp"
	"github.com/spf13/cobra"
	"strings"
	"testing"
)

func TestMCPToolsProfileFlags(t *testing.T) {
	for name, factory := range map[string]func() *cobra.Command{"serve": mcpServeCmd, "doctor": doctorCmd} {
		t.Run(name, func(t *testing.T) {
			cmd := factory()
			if flag := cmd.Flags().Lookup("tools"); flag == nil || flag.DefValue != gmcp.ToolsCore {
				t.Fatalf("tools flag default: %+v", flag)
			}
			for _, value := range []string{"", "invalid"} {
				cmd := factory()
				if err := cmd.ParseFlags([]string{"--tools=" + value}); err != nil {
					t.Fatal(err)
				}
				// Reject malformed selection before store access or any diagnostic.
				if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "use core or full") {
					t.Fatalf("profile %q rejected too late: %v", value, err)
				}
			}
		})
	}
}

func TestDoctorMCPCatalogReadOnlyCost(t *testing.T) {
	for _, profile := range []string{gmcp.ToolsCore, gmcp.ToolsFull} {
		cmd := doctorCmd()
		if err := cmd.ParseFlags([]string{"--tools", profile}); err != nil {
			t.Fatal(err)
		}
		selected, err := cmd.Flags().GetString("tools")
		if err != nil {
			t.Fatal(err)
		}
		got := checkMCPCatalog(selected)
		cost, err := gmcp.CatalogCost(profile)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("%s selected profile: %d estimated fixed tokens (%d UTF-8 bytes)", profile, cost.Tokens, cost.Bytes)
		if got.Status != "info" || !strings.Contains(got.Detail, want) || !strings.Contains(got.Hint, "not provider billing") || !strings.Contains(got.Hint, "does not inspect other processes' argv") {
			t.Fatalf("%+v", got)
		}
	}
	if got := checkMCPCatalog("invalid"); got.Status != "fail" {
		t.Fatalf("invalid profile: %+v", got)
	}
}

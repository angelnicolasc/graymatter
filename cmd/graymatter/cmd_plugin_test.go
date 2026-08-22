package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/plugin"
)

func reviewedManifest() plugin.PluginManifest {
	return plugin.PluginManifest{
		Name:        "hello",
		Version:     "1.2.3",
		Description: "greets people",
		Binary:      "/data/plugins/hello/bin/hello",
		SHA256:      strings.Repeat("ab", 32),
		Tools:       []plugin.MCPToolSpec{{Name: "hello_greet"}},
	}
}

// promptCmd wires a command with scripted stdin and captured output.
func promptCmd(stdin string) (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "install"}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	return cmd, &out
}

// TestConfirmPluginInstall_ShowsWhatIsBeingInstalled — H-06: a plugin used to
// install with no summary and no prompt at all, which is a poor way to grant
// code execution.
func TestConfirmPluginInstall_ShowsWhatIsBeingInstalled(t *testing.T) {
	cmd, out := promptCmd("y\n")
	if err := confirmPluginInstall(cmd, false)(reviewedManifest()); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	got := out.String()
	for _, want := range []string{"hello", "1.2.3", "hello_greet", strings.Repeat("ab", 32), "run code"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt does not mention %q:\n%s", want, got)
		}
	}
}

func TestConfirmPluginInstall_Answers(t *testing.T) {
	tests := []struct {
		stdin     string
		wantError bool
	}{
		{"y\n", false},
		{"Y\n", false},
		{"yes\n", false},
		{"  yes  \n", false},
		{"n\n", true},
		{"\n", true}, // bare enter is a no
		{"maybe\n", true},
		{"", true}, // EOF: no terminal to ask on
	}
	for _, tc := range tests {
		cmd, _ := promptCmd(tc.stdin)
		err := confirmPluginInstall(cmd, false)(reviewedManifest())
		if gotErr := err != nil; gotErr != tc.wantError {
			t.Errorf("answer %q: error = %v, want error = %v", tc.stdin, err, tc.wantError)
		}
	}
}

// TestConfirmPluginInstall_EOFIsRefusal — a script with no stdin must not be
// read as consent. It gets told to pass --yes.
func TestConfirmPluginInstall_EOFIsRefusal(t *testing.T) {
	cmd, _ := promptCmd("")
	err := confirmPluginInstall(cmd, false)(reviewedManifest())
	if !errors.Is(err, errInstallDeclined) {
		t.Fatalf("error = %v, want a decline", err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error = %v, want it to name the non-interactive flag", err)
	}
}

// TestConfirmPluginInstall_YesSkipsThePrompt keeps scripted installs working.
func TestConfirmPluginInstall_YesSkipsThePrompt(t *testing.T) {
	cmd, out := promptCmd("") // nothing to read; --yes must not read it
	if err := confirmPluginInstall(cmd, true)(reviewedManifest()); err != nil {
		t.Fatalf("confirm with --yes: %v", err)
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("--yes still prompted:\n%s", out.String())
	}
	// It should still say what it installed, and with which digest.
	if !strings.Contains(out.String(), strings.Repeat("ab", 32)) {
		t.Errorf("--yes printed no digest:\n%s", out.String())
	}
}

// TestPluginInstallCmd_HasSecurityFlags pins the two flags the docs promise.
func TestPluginInstallCmd_HasSecurityFlags(t *testing.T) {
	flags := pluginInstallCmd().Flags()
	for _, name := range []string{"yes", "insecure"} {
		if flags.Lookup(name) == nil {
			t.Errorf("plugin install is missing the --%s flag", name)
		}
	}
}

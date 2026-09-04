package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestHooksScopeDriftAndLegacyWarning(t *testing.T) {
	withHooksEnv(t)
	home, exe := t.TempDir(), filepath.Join(t.TempDir(), "graymatter")
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })
	settings := filepath.Join(home, ".claude", "settings.json")
	if _, err := upsertHookSettings(settings, exe, true, scopeGlobal); err != nil {
		t.Fatal(err)
	}
	if drift, err := hookSettingsDrift(settings, exe, scopeGlobal); err != nil || drift {
		t.Fatalf("fresh global install drift = %v, err = %v", drift, err)
	}
	if _, err := upsertHookSettings(settings, exe, true, scopeProject); err != nil {
		t.Fatal(err)
	}
	check := checkGlobalHooks()
	if check.Status != "warn" || !strings.Contains(check.Hint, "--scope global") {
		t.Fatalf("legacy global warning missing: %+v", check)
	}
	found := false
	for _, check := range runHooksDoctorChecks(settings, exe, scopeGlobal) {
		found = found || check.Name == "scope guard" && check.Status == "warn"
	}
	if !found {
		t.Fatal("hooks doctor omitted the legacy global warning")
	}
}

func TestHooksOwnership_DoesNotClaimForeignRunner(t *testing.T) {
	foreign := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "other-tool hooks run task"}}}
	if hookGroupIsOurs(foreign) {
		t.Fatal("foreign hooks runner was claimed as GrayMatter")
	}
	renamed := filepath.Join(t.TempDir(), "custom-bin")
	if edited := strings.Replace(hookCommand(renamed, "pre-compact"), " "+hooksCommandMarker, "", 1); !hookCommandIsOurs(edited, renamed) {
		t.Fatal("removing the managed marker hid a known installed binary from drift detection")
	}
	if legacy := filepath.ToSlash(renamed) + " graymatter hooks run pre-compact"; !hookCommandIsOurs(legacy, renamed) || hookBinaryPath(legacy) != renamed {
		t.Fatal("legacy malformed install is not safely replaceable")
	}
	next, _, _, _ := rewriteHookEvent([]any{foreign}, hookGroupsFor("graymatter", hooksEventPreCompact, scopeGlobal), true)
	after, _, _, _ := rewriteHookEvent(next, nil, false)
	if got := len(after.([]any)); got != 1 {
		t.Fatalf("foreign runner did not survive install+uninstall: %d groups", got)
	}
}

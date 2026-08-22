package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// traversalNames are the identifiers that must never reach the filesystem.
// Both separators are covered on every platform: on Windows filepath.Join
// treats '/' and '\\' alike, and a manifest published for Unix is still a
// manifest a Windows user can install.
func traversalNames() []string {
	names := []string{
		"..",
		".",
		"../escape",
		"..\\escape",
		"../../../gm-victim",
		"..\\..\\..\\gm-victim",
		"a/b",
		"a\\b",
		"./a",
		"sub/../../out",
		"/etc/cron.d",
		"C:\\Windows\\Temp",
		"\\\\server\\share",
		"",
		" ",
		"-leading-dash-is-fine-but-not-first",
		"name with spaces",
		"name;rm -rf",
		"name\x00truncated",
		strings.Repeat("a", 65),
	}
	return names
}

// TestPluginPath_RejectsTraversal is the shared regression test for H-04 and
// H-05: every one of these used to resolve to a path outside the plugins dir.
func TestPluginPath_RejectsTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")

	for _, name := range traversalNames() {
		if _, err := pluginPath(name, root); err == nil {
			t.Errorf("pluginPath(%q) was accepted; it must be rejected", name)
		}
	}
}

// TestPluginPath_AcceptsOrdinaryNames — the validator has to leave real plugin
// names alone, or the fix is just a regression.
func TestPluginPath_AcceptsOrdinaryNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")

	for _, name := range []string{
		"hello", "hello-world", "hello_world", "a", "A1", "0", "x-1_2-3",
		strings.Repeat("a", 64),
	} {
		got, err := pluginPath(name, root)
		if err != nil {
			t.Errorf("pluginPath(%q): %v", name, err)
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			t.Fatalf("abs: %v", err)
		}
		if want := filepath.Join(absRoot, name); got != want {
			t.Errorf("pluginPath(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestRemove_RefusesTraversal is H-04 end to end: a victim directory outside
// the store must survive `plugin remove` with a traversing name.
func TestRemove_RefusesTraversal(t *testing.T) {
	base := t.TempDir()
	pluginDir := filepath.Join(base, "data", "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}

	// The victim sits two levels above the plugins dir, like a sibling project
	// folder would.
	victim := filepath.Join(base, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	important := filepath.Join(victim, "important.txt")
	if err := os.WriteFile(important, []byte("do not delete"), 0o644); err != nil {
		t.Fatalf("write victim file: %v", err)
	}

	names := []string{
		"../victim",
		"..\\victim",
		"../../victim",
		filepath.Join("..", "victim"),
		victim, // absolute path
	}
	for _, name := range names {
		if err := Remove(name, pluginDir); err == nil {
			t.Errorf("Remove(%q) returned nil; it must refuse", name)
		}
		if _, err := os.Stat(important); err != nil {
			t.Fatalf("Remove(%q) destroyed a directory outside the store: %v", name, err)
		}
	}
}

// TestRemove_StillRemovesRealPlugins guards the happy path.
func TestRemove_StillRemovesRealPlugins(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")

	mp, _ := fakeManifest(t, dir, "keeper", []MCPToolSpec{{Name: "keeper_tool"}})
	if err := Install(mp, pluginDir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := Remove("keeper", pluginDir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "keeper")); !os.IsNotExist(err) {
		t.Errorf("plugin dir survived Remove: %v", err)
	}
}

// TestInstall_RefusesTraversalName is H-05: manifest.Name is chosen by whoever
// published the plugin, and it used to become a directory path verbatim.
func TestInstall_RefusesTraversalName(t *testing.T) {
	for _, name := range []string{
		"../pwned-outside-plugins",
		"..\\pwned-outside-plugins",
		"../../pwned",
		"sub/nested",
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			pluginDir := filepath.Join(base, "data", "plugins")

			binName := "bin"
			if runtime.GOOS == "windows" {
				binName += ".exe"
			}
			binPath := filepath.Join(base, binName)
			if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatalf("write bin: %v", err)
			}

			data, err := json.Marshal(PluginManifest{
				Name:    name,
				Version: "1.0.0",
				Binary:  binPath,
				Tools:   []MCPToolSpec{{Name: "evil_tool"}},
				// A correct digest, so the traversing name is the only thing
				// left for Install to object to.
				SHA256: mustHash(t, binPath),
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			mp := filepath.Join(base, "manifest.json")
			if err := os.WriteFile(mp, data, 0o644); err != nil {
				t.Fatalf("write manifest: %v", err)
			}

			if err := Install(mp, pluginDir); err == nil {
				t.Error("Install accepted a traversing manifest name")
			}

			// Nothing may have been written anywhere under base except the
			// two files this test created itself.
			var strays []string
			_ = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil //nolint:nilerr // a missing subtree is the expected outcome
				}
				if path == mp || path == binPath {
					return nil
				}
				strays = append(strays, path)
				return nil
			})
			if len(strays) > 0 {
				t.Errorf("Install wrote files despite refusing: %v", strays)
			}
		})
	}
}

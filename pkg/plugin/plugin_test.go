package plugin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// buildEchoPlugin compiles a minimal plugin binary that echoes the tool name.
// Skipped if the Go compiler is unavailable.
func buildEchoPlugin(t *testing.T, dir string) string {
	t.Helper()

	goExec, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go compiler not in PATH, skipping compile test")
	}

	src := `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type req  struct { Tool  string         ` + "`json:\"tool\"`" + `; Input map[string]any ` + "`json:\"input\"`" + ` }
type resp struct { Output string ` + "`json:\"output\"`" + `; Error string ` + "`json:\"error,omitempty\"`" + ` }

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() { os.Exit(1) }
	var r req
	if err := json.Unmarshal(scanner.Bytes(), &r); err != nil { os.Exit(1) }
	out, _ := json.Marshal(resp{Output: "echo:" + r.Tool})
	fmt.Println(string(out))
}
`
	srcFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcFile, []byte(src), 0o644); err != nil {
		t.Fatalf("write plugin src: %v", err)
	}

	binName := "echo-plugin"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(dir, binName)

	cmd := exec.Command(goExec, "build", "-o", binPath, srcFile)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin binary: %v\n%s", err, out)
	}
	return binPath
}

// fakeManifest writes a manifest JSON file pointing to a fake (existing) binary.
func fakeManifest(t *testing.T, dir, name string, tools []MCPToolSpec) (manifestPath, binPath string) {
	t.Helper()
	binPath = filepath.Join(dir, name+"-bin")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	m := PluginManifest{
		Name:    name,
		Version: "1.0.0",
		Binary:  binPath,
		Tools:   tools,
	}
	data, _ := json.Marshal(m)
	manifestPath = filepath.Join(dir, name+".json")
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return manifestPath, binPath
}

func TestInstall_LocalManifest(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")

	mp, _ := fakeManifest(t, dir, "hello", []MCPToolSpec{{Name: "hello_greet", Description: "Greets."}})
	if err := Install(mp, pluginDir); err != nil {
		t.Fatalf("Install: %v", err)
	}

	saved := filepath.Join(pluginDir, "hello", "manifest.json")
	if _, err := os.Stat(saved); err != nil {
		t.Fatalf("manifest not saved at %s: %v", saved, err)
	}

	// Saved manifest must be valid JSON with the right name.
	data, _ := os.ReadFile(saved)
	var got PluginManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal saved manifest: %v", err)
	}
	if got.Name != "hello" {
		t.Errorf("name = %q, want %q", got.Name, "hello")
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "hello_greet" {
		t.Errorf("tools = %v", got.Tools)
	}
}

func TestInstall_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")

	m := PluginManifest{Name: "broken", Version: "1.0.0", Binary: filepath.Join(dir, "nonexistent"), Tools: []MCPToolSpec{{Name: "t"}}}
	data, _ := json.Marshal(m)
	mp := filepath.Join(dir, "broken.json")
	_ = os.WriteFile(mp, data, 0o644)

	if err := Install(mp, pluginDir); err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
}

func TestInstall_MissingName(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")

	m := map[string]any{"version": "1.0.0", "binary": "/some/bin"}
	data, _ := json.Marshal(m)
	mp := filepath.Join(dir, "noname.json")
	_ = os.WriteFile(mp, data, 0o644)

	if err := Install(mp, pluginDir); err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
}

func TestInstall_RelativeBinary(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")

	binName := "rel-bin"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	_ = os.WriteFile(filepath.Join(dir, binName), []byte("#!/bin/sh\n"), 0o755)

	m := PluginManifest{Name: "rel-test", Version: "1.0.0", Binary: binName, Tools: []MCPToolSpec{{Name: "rel_tool"}}}
	data, _ := json.Marshal(m)
	mp := filepath.Join(dir, "rel-test.json")
	_ = os.WriteFile(mp, data, 0o644)

	if err := Install(mp, pluginDir); err != nil {
		t.Fatalf("Install with relative binary: %v", err)
	}

	plugins, _ := List(pluginDir)
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
	if !filepath.IsAbs(plugins[0].Binary) {
		t.Errorf("stored binary path should be absolute, got %q", plugins[0].Binary)
	}
}

func TestList_Empty(t *testing.T) {
	dir := t.TempDir()
	plugins, err := List(filepath.Join(dir, "plugins"))
	if err != nil {
		t.Fatalf("List on missing dir: %v", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("expected 0 plugins, got %d", len(plugins))
	}
}

func TestList_AfterInstall(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")

	for _, name := range []string{"alpha", "beta"} {
		mp, _ := fakeManifest(t, dir, name, []MCPToolSpec{{Name: name + "_tool"}})
		if err := Install(mp, pluginDir); err != nil {
			t.Fatalf("Install %s: %v", name, err)
		}
	}

	plugins, err := List(pluginDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(plugins))
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")

	mp, _ := fakeManifest(t, dir, "rm-test", []MCPToolSpec{{Name: "rm_tool"}})
	_ = Install(mp, pluginDir)

	if err := Remove("rm-test", pluginDir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	plugins, _ := List(pluginDir)
	for _, p := range plugins {
		if p.Name == "rm-test" {
			t.Fatal("plugin still listed after Remove")
		}
	}
}

func TestRemove_NotInstalled(t *testing.T) {
	dir := t.TempDir()
	if err := Remove("ghost", dir); err == nil {
		t.Fatal("expected error removing non-existent plugin")
	}
}

func TestFindByTool(t *testing.T) {
	manifests := []PluginManifest{
		{Name: "a", Tools: []MCPToolSpec{{Name: "a_tool"}, {Name: "shared_tool"}}},
		{Name: "b", Tools: []MCPToolSpec{{Name: "b_tool"}}},
	}

	if m := FindByTool("a_tool", manifests); m == nil || m.Name != "a" {
		t.Fatalf("FindByTool(a_tool): got %v", m)
	}
	if m := FindByTool("b_tool", manifests); m == nil || m.Name != "b" {
		t.Fatalf("FindByTool(b_tool): got %v", m)
	}
	if m := FindByTool("missing", manifests); m != nil {
		t.Fatalf("FindByTool(missing): expected nil, got %v", m)
	}
}

func TestCall_EchoPlugin(t *testing.T) {
	srcDir := t.TempDir()
	binPath := buildEchoPlugin(t, srcDir)

	manifest := PluginManifest{
		Name:   "echo",
		Binary: binPath,
		Tools:  []MCPToolSpec{{Name: "echo_hello"}},
	}

	resp, err := Call(context.Background(), manifest, "echo_hello", map[string]any{"msg": "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Output != "echo:echo_hello" {
		t.Errorf("output = %q, want %q", resp.Output, "echo:echo_hello")
	}
	if resp.Error != "" {
		t.Errorf("unexpected error field: %q", resp.Error)
	}
}

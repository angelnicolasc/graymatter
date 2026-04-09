// Package plugin implements the GrayMatter plugin system.
// Plugins are Go binaries that speak a simple JSON line protocol over stdin/stdout.
// They extend the MCP tool surface without modifying graymatter core.
//
// Protocol (each line is a JSON object terminated by \n):
//
//	→ stdin:  {"tool":"<name>","input":{...}}
//	← stdout: {"output":"...","error":"..."}
//
// The MCP server spawns the plugin binary per tool call and kills it after
// a 30-second timeout.
package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const pluginTimeout = 30 * time.Second

// MCPToolSpec is the tool definition a plugin registers in the MCP server.
type MCPToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PluginManifest describes an installed plugin.
type PluginManifest struct {
	Name        string        `json:"name"`
	Version     string        `json:"version"`
	Description string        `json:"description"`
	Binary      string        `json:"binary"` // absolute path to executable
	Tools       []MCPToolSpec `json:"tools"`
}

// PluginRequest is the JSON object written to the plugin's stdin.
type PluginRequest struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
}

// PluginResponse is the JSON object read from the plugin's stdout.
type PluginResponse struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// Install downloads and registers a plugin from url into pluginDir.
// url may be:
//   - A local file path to a manifest JSON file (for development)
//   - An HTTPS URL to a manifest JSON file
//
// The manifest must contain a valid "binary" path relative to the manifest
// location, or an absolute path.
func Install(url, pluginDir string) error {
	// Fetch manifest JSON.
	var data []byte
	var err error

	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		data, err = fetchHTTP(url)
	} else {
		data, err = os.ReadFile(url)
	}
	if err != nil {
		return fmt.Errorf("plugin install: fetch manifest from %q: %w", url, err)
	}

	var manifest PluginManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("plugin install: parse manifest: %w", err)
	}
	if manifest.Name == "" {
		return fmt.Errorf("plugin install: manifest missing required field: name")
	}
	if manifest.Binary == "" {
		return fmt.Errorf("plugin install: manifest missing required field: binary")
	}

	// If binary path is relative, resolve it relative to the manifest location.
	if !filepath.IsAbs(manifest.Binary) {
		base := filepath.Dir(url)
		if strings.HasPrefix(url, "http") {
			// For HTTP installs, binary must be absolute or pre-resolved.
			return fmt.Errorf("plugin install: binary path must be absolute for HTTP manifests")
		}
		manifest.Binary = filepath.Join(base, manifest.Binary)
	}

	// Verify the binary exists and is executable.
	if _, err := os.Stat(manifest.Binary); err != nil {
		return fmt.Errorf("plugin install: binary not found at %q: %w", manifest.Binary, err)
	}

	// Persist manifest.
	dir := filepath.Join(pluginDir, manifest.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("plugin install: mkdir: %w", err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		return fmt.Errorf("plugin install: write manifest: %w", err)
	}

	return nil
}

// List returns all installed plugins in pluginDir.
func List(pluginDir string) ([]PluginManifest, error) {
	entries, err := os.ReadDir(pluginDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugin list: %w", err)
	}

	var manifests []PluginManifest
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(pluginDir, entry.Name(), "manifest.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue // skip corrupt plugin dirs
		}
		var m PluginManifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		manifests = append(manifests, m)
	}
	return manifests, nil
}

// Remove uninstalls a plugin by name from pluginDir.
func Remove(name, pluginDir string) error {
	dir := filepath.Join(pluginDir, name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("plugin %q not installed", name)
	}
	return os.RemoveAll(dir)
}

// Call invokes a plugin binary for the given tool name with input, returning
// the plugin's response. It starts the binary as a subprocess, writes the
// request to stdin, reads the response from stdout, and kills the process
// after pluginTimeout (30s).
func Call(ctx context.Context, manifest PluginManifest, toolName string, input map[string]any) (*PluginResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, pluginTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, manifest.Binary) //nolint:gosec
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin call: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin call: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin call: start binary %q: %w", manifest.Binary, err)
	}

	// Write request JSON line.
	req := PluginRequest{Tool: toolName, Input: input}
	reqData, err := json.Marshal(req)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("plugin call: marshal request: %w", err)
	}
	reqData = append(reqData, '\n')
	if _, err := stdin.Write(reqData); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("plugin call: write request: %w", err)
	}
	_ = stdin.Close()

	// Read response JSON line.
	respData, err := readLine(stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("plugin call: read response: %w", err)
	}
	_ = cmd.Wait()

	var resp PluginResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("plugin call: parse response: %w", err)
	}
	return &resp, nil
}

// FindByTool returns the manifest of the plugin that registered toolName,
// or nil if no plugin handles it.
func FindByTool(toolName string, manifests []PluginManifest) *PluginManifest {
	for i := range manifests {
		for _, t := range manifests[i].Tools {
			if t.Name == toolName {
				return &manifests[i]
			}
		}
	}
	return nil
}

// --- internal helpers ---

func fetchHTTP(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:gosec,noctx
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func readLine(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	if scanner.Scan() {
		return scanner.Bytes(), nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

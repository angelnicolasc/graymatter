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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const pluginTimeout = 30 * time.Second

// maxManifestBytes caps a fetched manifest. A manifest is a few hundred bytes
// of JSON; a remote host that answers with an endless body should not be able
// to exhaust memory here.
const maxManifestBytes = 1 << 20 // 1 MiB

// binSubdir is where Install copies the plugin executable, under the plugin's
// own directory.
const binSubdir = "bin"

// sha256Re matches a lowercase hex SHA-256 digest.
var sha256Re = regexp.MustCompile(`^[a-f0-9]{64}$`)

// pluginNameRe is the whitelist for a plugin identifier. A plugin name becomes
// a directory name under the plugins dir, and filepath.Join cleans a path
// without containing it: Join(pluginsDir, "../../../elsewhere") happily points
// outside the store. Both Install (which takes the name from a manifest
// written by whoever published the plugin) and Remove (which takes it from the
// command line) go through here.
//
// Letters, digits, '-' and '_', starting with a letter or digit. No
// separators, no dots, so ".." cannot be spelled at all.
var pluginNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// pluginPath resolves the on-disk directory for a plugin name.
//
// The regex is the real gate; the containment check below is defence in depth,
// so that loosening the pattern later cannot quietly reintroduce traversal. It
// compares cleaned absolute paths, which does not follow symlinks — a
// pre-existing symlink inside the plugins dir is out of scope here, since
// anyone who can plant one can write to the plugins dir directly.
func pluginPath(name, pluginDir string) (string, error) {
	if !pluginNameRe.MatchString(name) {
		return "", fmt.Errorf(
			"plugin %q: invalid name (letters, digits, '-' and '_' only, "+
				"starting with a letter or digit, at most 64 characters)", name)
	}

	absRoot, err := filepath.Abs(pluginDir)
	if err != nil {
		return "", fmt.Errorf("plugin %q: resolve plugins dir: %w", name, err)
	}
	dir := filepath.Join(absRoot, name)
	if dir == absRoot || !strings.HasPrefix(dir, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("plugin %q: resolved path escapes the plugins directory", name)
	}
	return dir, nil
}

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

	// SHA256 is the lowercase hex digest of the plugin executable. It is
	// required: nothing else ties a manifest to the bytes it will run, and
	// Call refuses to exec a plugin whose binary no longer matches.
	//
	// In an installed manifest this describes the copy under the plugin's own
	// bin/ directory, which is byte-identical to the one that was verified at
	// install time.
	SHA256 string `json:"sha256"`
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

// InstallOption customises Install. The zero set is the strict path: HTTPS
// only, mandatory checksum, no prompt.
type InstallOption func(*installOptions)

type installOptions struct {
	allowInsecureHTTP bool
	confirm           func(PluginManifest) error
}

// WithInsecureHTTP allows fetching a manifest over plaintext http://.
//
// A manifest names the executable that will run on the user's machine, and
// http:// lets anyone on the path rewrite it. This exists for local
// development against a throwaway server, nothing else.
func WithInsecureHTTP() InstallOption {
	return func(o *installOptions) { o.allowInsecureHTTP = true }
}

// WithConfirm registers a callback invoked once the manifest is fetched,
// validated and checksum-verified, and before anything is written to disk.
// Returning an error aborts the install and leaves no trace.
//
// The CLI uses this to show what is about to be installed — name, version,
// tools, digest — and ask. Installing a plugin is granting code execution, so
// it should look like a decision rather than a download.
func WithConfirm(fn func(PluginManifest) error) InstallOption {
	return func(o *installOptions) { o.confirm = fn }
}

// Install downloads and registers a plugin from url into pluginDir.
// url may be:
//   - A local file path to a manifest JSON file (for development)
//   - An HTTPS URL to a manifest JSON file
//
// The manifest must carry a "sha256" digest of the executable and a "binary"
// path — absolute, or relative to a local manifest's own directory. The
// executable is verified against the digest and then copied into
// <pluginDir>/<name>/bin/, and the stored manifest points at that copy: what
// runs later is the reviewed bytes, from inside the store, not whatever
// happens to live at an external path by then.
func Install(url, pluginDir string, opts ...InstallOption) error {
	var cfg installOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	data, err := fetchManifest(url, cfg.allowInsecureHTTP)
	if err != nil {
		return err
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
	manifest.SHA256 = strings.ToLower(strings.TrimSpace(manifest.SHA256))
	if manifest.SHA256 == "" {
		return fmt.Errorf(
			"plugin install: manifest missing required field: sha256 " +
				"(the hex SHA-256 of the plugin executable; nothing else ties this manifest to the code it runs)")
	}
	if !sha256Re.MatchString(manifest.SHA256) {
		return fmt.Errorf("plugin install: sha256 %q is not a 64-character hex digest", manifest.SHA256)
	}

	// The name comes from a JSON file written by whoever published the plugin,
	// and it used to become a directory path verbatim. Validate it before any
	// filesystem call, so a refused install leaves nothing behind.
	dir, err := pluginPath(manifest.Name, pluginDir)
	if err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}

	// If binary path is relative, resolve it relative to the manifest location.
	source := manifest.Binary
	if !filepath.IsAbs(source) {
		if isRemoteURL(url) {
			// For remote installs, binary must be absolute or pre-resolved.
			return fmt.Errorf("plugin install: binary path must be absolute for remote manifests")
		}
		source = filepath.Join(filepath.Dir(url), source)
	}

	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("plugin install: binary not found at %q: %w", source, err)
	}
	if info.IsDir() {
		return fmt.Errorf("plugin install: binary %q is a directory", source)
	}

	got, err := fileSHA256(source)
	if err != nil {
		return fmt.Errorf("plugin install: hash binary: %w", err)
	}
	if got != manifest.SHA256 {
		return fmt.Errorf(
			"plugin install: binary %q does not match the manifest: sha256 is %s, manifest says %s",
			source, got, manifest.SHA256)
	}

	// The manifest the caller reviews is the one that will be stored, with the
	// binary already pointing inside the store.
	manifest.Binary = filepath.Join(dir, binSubdir, filepath.Base(source))
	if cfg.confirm != nil {
		if err := cfg.confirm(manifest); err != nil {
			return err
		}
	}

	binDir := filepath.Join(dir, binSubdir)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("plugin install: mkdir: %w", err)
	}
	// A failed install should not leave a half-populated plugin behind for
	// List to advertise.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	if err = copyExecutable(source, manifest.Binary); err != nil {
		return fmt.Errorf("plugin install: copy binary: %w", err)
	}
	// Re-hash the copy. Cheap, and it catches a truncated or racing write
	// before anything is registered.
	var copied string
	if copied, err = fileSHA256(manifest.Binary); err != nil {
		return fmt.Errorf("plugin install: hash installed binary: %w", err)
	}
	if copied != manifest.SHA256 {
		err = fmt.Errorf("plugin install: installed copy hashes to %s, expected %s", copied, manifest.SHA256)
		return err
	}

	manifestPath := filepath.Join(dir, "manifest.json")
	var manifestData []byte
	if manifestData, err = json.MarshalIndent(manifest, "", "  "); err != nil {
		return fmt.Errorf("plugin install: encode manifest: %w", err)
	}
	if err = os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
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
//
// The name is validated before it reaches os.RemoveAll. It used to be joined
// straight onto pluginDir, which made `graymatter plugin remove ../../../x` a
// recursive delete of any directory the user could reach — no undo, no
// recycle bin.
func Remove(name, pluginDir string) error {
	dir, err := pluginPath(name, pluginDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("plugin %q not installed", name)
	}
	return os.RemoveAll(dir)
}

// Call invokes a plugin binary for the given tool name with input, returning
// the plugin's response. It starts the binary as a subprocess, writes the
// request to stdin, reads the response from stdout, and kills the process
// after pluginTimeout (30s).
// It verifies the executable against the manifest's sha256 first. Install
// already checked it, but the file has been sitting on disk since then, and
// the check is the only thing standing between "a plugin was reviewed once"
// and "these bytes are the ones that were reviewed".
func Call(ctx context.Context, manifest PluginManifest, toolName string, input map[string]any) (*PluginResponse, error) {
	if err := VerifyBinary(manifest); err != nil {
		return nil, err
	}

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

// VerifyBinary reports whether the executable named by manifest still hashes
// to the digest recorded at install time.
//
// A manifest with no digest is refused rather than trusted: those come from
// installs predating the checksum requirement, and the fix for them is
// `graymatter plugin install` again.
func VerifyBinary(manifest PluginManifest) error {
	want := strings.ToLower(strings.TrimSpace(manifest.SHA256))
	if want == "" {
		return fmt.Errorf(
			"plugin %q: manifest has no sha256, so the binary cannot be verified; reinstall it",
			manifest.Name)
	}
	got, err := fileSHA256(manifest.Binary)
	if err != nil {
		return fmt.Errorf("plugin %q: hash binary: %w", manifest.Name, err)
	}
	if got != want {
		return fmt.Errorf(
			"plugin %q: binary %q hashes to %s but the manifest says %s; refusing to run it",
			manifest.Name, manifest.Binary, got, want)
	}
	return nil
}

// --- internal helpers ---

// fetchManifest reads a manifest from a local path or a remote URL.
//
// Plaintext http:// is refused by default: the manifest names the executable
// that will run on this machine, and anyone on the network path could rewrite
// it in transit.
func fetchManifest(url string, allowInsecureHTTP bool) ([]byte, error) {
	switch {
	case strings.HasPrefix(url, "https://"):
		data, err := fetchHTTP(url)
		if err != nil {
			return nil, fmt.Errorf("plugin install: fetch manifest from %q: %w", url, err)
		}
		return data, nil
	case strings.HasPrefix(url, "http://"):
		if !allowInsecureHTTP {
			return nil, fmt.Errorf(
				"plugin install: refusing to fetch a manifest over plaintext http from %q; "+
					"use https, or pass --insecure if you are testing against a local server", url)
		}
		data, err := fetchHTTP(url)
		if err != nil {
			return nil, fmt.Errorf("plugin install: fetch manifest from %q: %w", url, err)
		}
		return data, nil
	default:
		data, err := os.ReadFile(url)
		if err != nil {
			return nil, fmt.Errorf("plugin install: fetch manifest from %q: %w", url, err)
		}
		return data, nil
	}
}

func isRemoteURL(url string) bool {
	return strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")
}

// fileSHA256 returns the lowercase hex SHA-256 of the file at path, streaming
// it rather than reading it whole — a plugin binary can be tens of megabytes.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the path comes from a validated manifest
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyExecutable copies src to dst with the execute bit set. dst is truncated
// if it exists, which is what reinstalling a plugin should do.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is the manifest-declared binary, already hashed
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // dst is inside the plugins dir
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func fetchHTTP(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:gosec,noctx
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	// Bound the read: a hostile or broken server should not be able to stream
	// an endless body into memory.
	return io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
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

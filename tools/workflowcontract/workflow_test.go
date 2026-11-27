package workflowcontract

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const workflowDir = "../../.github/workflows"

func read(t *testing.T, name string) string {
	t.Helper()
	return readWorkflow(t, filepath.Join(workflowDir, name+".yml"))
}

func readWorkflow(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Git may check out YAML with CRLF on Windows. Keep every contract and
	// action pin exact while comparing the same source lines on every OS.
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

func TestReadWorkflowLineEndings(t *testing.T) {
	const source = "on:\n  pull_request:\npermissions:\n  contents: read\nsteps:\n  - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1\n"
	for _, ending := range []string{"\n", "\r\n"} {
		name := "LF"
		if ending == "\r\n" {
			name = "CRLF"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.yml")
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(source, "\n", ending)), 0o600); err != nil {
				t.Fatal(err)
			}
			if body := readWorkflow(t, path); body != source {
				t.Fatalf("workflow source or action pin changed after reading %s: %q", name, body)
			}
		})
	}
}

func contains(t *testing.T, body string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(body, fragment) {
			t.Errorf("missing workflow contract: %q", fragment)
		}
	}
}

func absent(t *testing.T, body string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if strings.Contains(body, fragment) {
			t.Errorf("unexpected workflow contract: %q", fragment)
		}
	}
}

func TestActionPins(t *testing.T) {
	expected := map[string]string{
		"actions/checkout":             "3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1",
		"actions/setup-go":             "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0",
		"actions/upload-artifact":      "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a # v7.0.1",
		"actions/download-artifact":    "3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c # v8.0.1",
		"actions/setup-node":           "820762786026740c76f36085b0efc47a31fe5020 # v7.0.0",
		"goreleaser/goreleaser-action": "f06c13b6b1a9625abc9e6e439d9c05a8f2190e94 # v7.2.3",
		"cloudflare/wrangler-action":   "953926a2e2182532811c01a25e53647d93bf07c0 # v4.1.3",
	}
	re := regexp.MustCompile("(?m)^\\s+(?:-\\s+)?uses: ([A-Za-z0-9_./-]+)@([^\\r\\n]+)$")
	files, err := filepath.Glob(filepath.Join(workflowDir, "*.yml"))
	if err != nil || len(files) != 8 {
		t.Fatalf("expected eight workflows, got %d: %v", len(files), err)
	}
	seen := make(map[string]bool)
	for _, file := range files {
		body := readWorkflow(t, file)
		for _, match := range re.FindAllStringSubmatch(body, -1) {
			want, ok := expected[match[1]]
			if !ok {
				t.Errorf("%s uses unreviewed action %s", file, match[1])
			} else if match[2] != want {
				t.Errorf("%s: %s@%s, want %s", file, match[1], match[2], want)
			}
			seen[match[1]] = true
		}
	}
	for action := range expected {
		if !seen[action] {
			t.Errorf("reviewed action %s is unused", action)
		}
	}
}

func TestGoCacheInputs(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(workflowDir, "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	count, install := 0, 0
	for _, file := range files {
		body := readWorkflow(t, file)
		lines := strings.Split(body, "\n")
		for i, line := range lines {
			if !strings.Contains(line, "uses: actions/setup-go@") {
				continue
			}
			count++
			end := i + 1
			for end < len(lines) && !strings.HasPrefix(lines[end], "      - ") {
				end++
			}
			step := strings.Join(lines[i:end], "\n")
			if filepath.Base(file) == "release.yml" && strings.Contains(strings.Join(lines[:i], "\n"), "  install-smoke:") {
				install++
				contains(t, step, "cache: false")
				absent(t, step, "cache-dependency-path:")
			} else {
				contains(t, step, "cache-dependency-path: |", "\n            go.sum", "\n            cmd/graymatter/go.sum")
			}
		}
	}
	if count == 0 || install != 1 {
		t.Errorf("setup-go inventory: %d total, %d without checkout", count, install)
	}
}

func TestCoverageUnionContract(t *testing.T) {
	body := read(t, "ci")
	os := "$" + "{{ matrix.os }}"
	goVersion := "$" + "{{ matrix.go }}"
	contains(t, body,
		"os: [ubuntu-latest, macos-latest, windows-latest]",
		"go: [\"1.25\", \"1.26.11\"]",
		"name: coverage-"+os+"-go"+goVersion,
		"coverage-core."+os+".go"+goVersion+".out",
		"coverage-cli."+os+".go"+goVersion+".out",
		"for os in ubuntu-latest macos-latest windows-latest; do",
		"for version in 1.25 1.26.11; do",
		"if [ ! -s \"$profile\" ]; then",
		"merge-multiple: true",
		"digest-mismatch: error",
		"core+=(\"$profile\")",
		"cli+=(\"$profile\")",
		"name: coverage-union",
		"archive: true",
		"Check workflow contracts",
	)
	absent(t, body, "name: coverage-"+os+"\n", "coverage-core."+os+".out", "coverage-cli."+os+".out")
}

func TestMaintenanceSmokeSafetyAndScope(t *testing.T) {
	body := read(t, "maintenance-smoke")
	contains(t, body,
		"on:\n  pull_request:\n  workflow_dispatch:",
		"permissions:\n  contents: read",
		"  snapshot:", "  docs:", "  fuzz:", "  artifacts-mutation:",
		"fetch-depth: 0", "go-version: \"1.26.11\"", "version: v2.17.1",
		"install-only: true", "python3 .github/scripts/check_goreleaser.py", "args: release --snapshot --clean --skip=sign",
		"(\"linux\", \"amd64\"), (\"linux\", \"arm64\")",
		"(\"darwin\", \"amd64\"), (\"darwin\", \"arm64\")",
		"(\"windows\", \"amd64\")",
		"node-version: 22", "cache-dependency-path: www/package-lock.json",
		"wranglerVersion: 4.125.0", "command: deploy --dry-run --outdir",
		"FuzzTokenize -fuzztime 10s", "FuzzUnmarshalFact -fuzztime 10s",
		"FuzzKeywordScore -fuzztime 10s", "FuzzRenderBlock -fuzztime 10s",
		"gremlins/cmd/gremlins@v0.5.0",
		"unleash --dry-run --output mutation-report.json",
		"python3 -m json.tool mutation-report.json",
		"name: mutation-report-smoke", "name: fuzz-corpus-fixture",
		"Download corpus fixture", "Verify corpus integrity",
		"digest-mismatch: error",
	)
	absent(t, body, "  push:", "  schedule:", "pull_request_target:", "contents: write",
		"id-token: write", "secrets.", "TAP_GITHUB_TOKEN", "CLOUDFLARE_API_TOKEN")
}

func TestProductionGuards(t *testing.T) {
	release := read(t, "release")
	docs := read(t, "deploy-docs")
	fuzz := read(t, "fuzz")
	mutation := read(t, "mutation")
	contains(t, release, "on:\n  push:\n    tags:", "Preserve package-manager manifests for signed publication",
		"Publish the CLI submodule tag", "Warm the public proxy and wait for the checksum database",
		"version: v2.17.1", "README must advertise the release being published")
	absent(t, release, "  pull_request:")
	contains(t, docs, "branches: [main]", "node-version: 22",
		"cache: npm", "cache-dependency-path: www/package-lock.json",
		"wranglerVersion: 4.125.0", "command: deploy", "command: versions upload",
		"github.ref == 'refs/heads/main' && github.event_name != 'pull_request'",
		"github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository")
	absent(t, docs, "  pull_request_target:")
	for _, name := range []string{"FuzzTokenize", "FuzzUnmarshalFact", "FuzzKeywordScore", "FuzzRenderBlock"} {
		contains(t, fuzz, "-fuzz "+name+" -fuzztime 60s")
	}
	contains(t, fuzz, "pkg/memory/testdata/fuzz",
		"cmd/graymatter/internal/contextblock/testdata/fuzz", "archive: true")
	contains(t, mutation, "continue-on-error: true", "python3 -m json.tool mutation-report.json",
		"if-no-files-found: error")
	absent(t, mutation, "--exclude-files")
	mcpReg := read(t, "mcp-registry")
	contains(t, mcpReg, "workflow_dispatch:", "packages: write", "id-token: write",
		"mcp-publisher login github-oidc", "mcp-publisher publish")
	absent(t, mcpReg, "  pull_request:", "pull_request_target:")
}

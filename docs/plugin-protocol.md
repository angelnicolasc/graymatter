# GrayMatter Plugin Protocol

Plugins extend the GrayMatter MCP tool surface without modifying the core binary. Each plugin is an independent executable that communicates with the MCP server over `stdin`/`stdout` using a newline-delimited JSON protocol.

---

## Protocol contract

### Request (written to plugin stdin)

```json
{"tool":"<tool-name>","input":{...}}\n
```

| Field   | Type              | Description                              |
|---------|-------------------|------------------------------------------|
| `tool`  | `string`          | The MCP tool name being invoked.         |
| `input` | `object` (any)    | Arbitrary key-value pairs from the call. |

### Response (read from plugin stdout)

```json
{"output":"...","error":"..."}\n
```

| Field    | Type     | Description                                              |
|----------|----------|----------------------------------------------------------|
| `output` | `string` | The tool result text, surfaced to the MCP caller.        |
| `error`  | `string` | Optional. Non-empty value marks the call as failed.      |

- Each exchange is exactly **one request line → one response line**.
- The plugin binary is started fresh for each tool call and killed after **30 seconds**.
- Anything written to `stderr` is discarded by the MCP server.

---

## Manifest format

Install a plugin by providing a manifest JSON file:

```json
{
  "name":        "hello",
  "version":     "1.0.0",
  "description": "Greets a person by name.",
  "binary":      "./plugin-hello",
  "sha256":      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "tools": [
    {
      "name":        "hello_greet",
      "description": "Greet a person. Input: {\"name\": \"<string>\"}."
    }
  ]
}
```

| Field         | Type           | Required | Description                                                    |
|---------------|----------------|----------|----------------------------------------------------------------|
| `name`        | `string`       | yes      | Unique plugin identifier (alphanumeric + hyphens).             |
| `version`     | `string`       | no       | Semver string for informational display.                       |
| `description` | `string`       | no       | Human-readable description shown in `plugin list`.             |
| `binary`      | `string`       | yes      | Path to the plugin executable. Relative paths are resolved relative to the manifest file location. Remote installs require an absolute path. |
| `sha256`      | `string`       | yes      | Lowercase hex SHA-256 of the executable. Verified before install and again before every call. |
| `tools`       | `[]ToolSpec`   | no       | MCP tools this plugin registers. Each entry has `name` and `description`. |

### Integrity and trust

Installing a plugin grants code execution on the user's machine, so the install
path is deliberately strict:

- **`sha256` is required.** Nothing else ties a manifest to the bytes it will
  run. The digest is checked before the install is recorded, and again before
  every call — a binary swapped afterwards does not get to run.
- **The executable is copied into the store.** After install, `binary` points at
  `<data-dir>/plugins/<name>/bin/<file>`. A manifest can name any executable on
  the machine, but what runs later is the copy that was verified, not whatever
  is at the original path by then.
- **Manifests are fetched over HTTPS only.** `http://` needs `--insecure`, which
  exists for testing against a local server.
- **`graymatter plugin install` asks first**, showing name, version, tools and
  digest. `--yes` skips the prompt for scripted installs.

Computing the digest:

```bash
sha256sum ./plugin-hello          # Linux / macOS
Get-FileHash ./plugin-hello.exe   # Windows PowerShell
```

If you get it wrong, the error prints the digest the file actually has, which
is the value to paste back into the manifest.

---

## Lifecycle

> **Status.** `install`, `list` and `remove` are wired up today. The MCP server
> does not yet register plugin tools, so nothing in the shipped binary reaches
> `Call` — the diagram below describes the intended path, not a code path you
> can exercise right now.

```
graymatter mcp serve
│
├─ receives tool call for "hello_greet"
│
├─ FindByTool("hello_greet", manifests)  →  PluginManifest{Binary: ".../plugins/hello/bin/plugin-hello"}
│
├─ VerifyBinary(manifest)  →  sha256 must still match the manifest
│
├─ exec.CommandContext(ctx, ".../plugins/hello/bin/plugin-hello")
│    stdin  ← {"tool":"hello_greet","input":{"name":"Alice"}}\n
│    stdout → {"output":"Hello, Alice!"}\n
│    30-second context timeout
│
└─ return CallToolResult{Text: "Hello, Alice!"}
```

---

## Writing a plugin

Plugins can be written in any language that can read a line from stdin and write a line to stdout.

### Go (reference implementation)

See [`examples/plugin-hello/main.go`](../examples/plugin-hello/main.go).

Build:

```bash
CGO_ENABLED=0 go build -o plugin-hello ./examples/plugin-hello
```

Install:

```bash
# The committed manifest ships a placeholder digest; put the real one in first.
sha256sum ./plugin-hello
graymatter plugin install examples/plugin-hello/manifest.json
```

### Shell script

```bash
#!/usr/bin/env bash
read -r line
tool=$(echo "$line" | python3 -c "import sys,json; print(json.load(sys.stdin)['tool'])")
echo '{"output":"invoked: '"$tool"'"}'
```

### Python

```python
#!/usr/bin/env python3
import json, sys

req = json.loads(sys.stdin.readline())
if req["tool"] == "my_tool":
    result = {"output": f"Hello from Python! tool={req['tool']}"}
else:
    result = {"error": f"unknown tool: {req['tool']}"}
print(json.dumps(result), flush=True)
```

---

## Error handling

- If the plugin writes `{"error":"<message>"}`, the MCP server surfaces it as a tool error.
- If the plugin exits non-zero or times out, the MCP server returns an internal error.
- If the plugin binary is not found or not executable, `plugin call` returns immediately with an error.

---

## Security considerations

- Plugin binaries run with the **same permissions** as the `graymatter` process.
- Only install plugins from sources you trust.
- HTTP manifest installs require the `binary` path to be absolute (the binary itself is not downloaded — only the manifest is fetched).
- Plugin binaries are not sandboxed; they can access the filesystem and network.

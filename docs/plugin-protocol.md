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
| `binary`      | `string`       | yes      | Path to the plugin executable. Relative paths are resolved relative to the manifest file location. HTTP installs require an absolute path. |
| `tools`       | `[]ToolSpec`   | no       | MCP tools this plugin registers. Each entry has `name` and `description`. |

---

## Lifecycle

```
graymatter mcp serve
│
├─ receives tool call for "hello_greet"
│
├─ FindByTool("hello_greet", manifests)  →  PluginManifest{Binary: "/path/plugin-hello"}
│
├─ exec.CommandContext(ctx, "/path/plugin-hello")
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

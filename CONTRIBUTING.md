# Contributing to GrayMatter

Thanks for taking the time. GrayMatter is small and intentional — contributions that keep it that way are the most welcome.

---

## What we're looking for

**Great fits:**
- Bug fixes with a failing test that reproduces the issue
- Coverage improvements for uncovered code paths
- New embedding backend adapters (implement `VectorStore`, drop in)
- Performance improvements with `go test -bench` numbers before/after
- Docs corrections — especially if something in the README doesn't match the code

**Out of scope:**
- External service dependencies in the core library (`pkg/`)
- Changes that require CGO
- Features that belong in the CLI binary being added to the core module

When in doubt, open an issue first. A quick description of what you're building and why saves everyone time.

---

## Setup

**Requirements:** Go 1.22+, no CGO, no Docker, no external services.

```bash
git clone https://github.com/angelnicolasc/graymatter
cd graymatter
go work sync   # resolves both modules: root + cmd/graymatter
```

The repo uses a `go.work` workspace with two modules:

| Module | Path | What's in it |
|--------|------|--------------|
| `github.com/angelnicolasc/graymatter` | `.` | Core library — memory, embedding, storage |
| `github.com/angelnicolasc/graymatter/cmd/graymatter` | `./cmd/graymatter` | CLI, TUI, MCP server, REST server, plugins |

Keep them separate. CLI dependencies (bubbletea, cobra, etc.) must not appear in the root `go.mod`.

---

## Running tests

```bash
# Core library — no network, no LLM, no Docker
go test -count=1 -timeout=120s ./pkg/memory/...

# CLI and server packages
cd cmd/graymatter
go test -count=1 -timeout=120s ./internal/...
```

All tests use `t.TempDir()` with injected stubs. No real embedding model is required — tests that need one use a fixed-vector stub or keyword mode.

**Coverage gate:** the CI enforces ≥ 70% statement coverage on `pkg/memory`. If your change drops coverage, add tests for the new code paths before submitting.

```bash
# Check coverage locally before pushing
go test -coverprofile=coverage.out -covermode=atomic ./pkg/memory/...
go tool cover -func=coverage.out | grep '^total:'
```

**Fuzz targets:** seed them, don't break them.

```bash
# Run a fuzz target for 30 seconds locally
go test -fuzz=FuzzTokenize -fuzztime=30s ./pkg/memory/...
```

---

## Code conventions

- **No global state** in the core library. Everything goes through `StoreConfig` or function arguments.
- **Errors propagate, never print.** Internal helpers return `error`; callers decide what to log.
- **Context everywhere.** Any function that does I/O must accept `context.Context` as its first parameter.
- **No `init()` side effects** that affect behavior — only for package-level var initialization.
- Standard formatting: `gofmt` / `goimports`. The CI runs `go vet ./...`; keep it clean.

---

## Submitting a PR

1. Fork → branch off `main` → make your changes.
2. Add or update tests. The CI will enforce coverage.
3. Run the full test suite locally:
   ```bash
   go test -count=1 -timeout=120s ./pkg/... && \
   cd cmd/graymatter && go test -count=1 -timeout=120s ./internal/...
   ```
4. Open the PR against `main`. Keep the title concise (`fix:`, `feat:`, `test:`, `docs:`, `refactor:`).

PR descriptions don't need a template. Just explain what the change does and why. If it fixes a bug, link the issue or include a one-liner that shows the broken behavior.

---

## Adding a vector backend

`VectorStore` is the extension point. Implement the interface and pass it via `StoreConfig.VectorBackend`:

```go
type VectorStore interface {
    AddDocument(ctx context.Context, collection, id, content string, embedding []float32, metadata map[string]string) error
    Query(ctx context.Context, collection string, embedding []float32, n int) ([]VectorResult, error)
    EnsureCollection(collection string) error
    Close() error
}
```

Implementations must be safe for concurrent use. See `pkg/memory/vectorstore.go` for the chromem-go reference adapter.

---

## License

By contributing, you agree that your changes will be licensed under the same [MIT License](LICENSE) as the rest of the project.

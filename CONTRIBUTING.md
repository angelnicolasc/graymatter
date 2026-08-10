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

**Requirements:** Go 1.23+, no Docker, no external services.

The workspace declares `go 1.23.0`, so an older toolchain either fetches 1.23
or refuses outright when `GOTOOLCHAIN=local` — which is what container images
set. The shipped binary is built without CGO; that constraint is about the
release artifact, not about your machine. See the note on `-race` below.

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
# Core library and the public API surface — no network, no LLM, no Docker
go test -count=1 -timeout=120s ./...

# CLI, TUI, server and the command entrypoint
cd cmd/graymatter
go test -count=1 -timeout=300s ./...
```

Use `./...` rather than naming packages. Both commands used to enumerate
subtrees, which quietly excluded the root package and `cmd/graymatter` itself,
so tests living there passed locally and were never run by anyone else.

### The race detector

CI runs every one of those packages under `-race`. The commands above do not,
so a change can pass locally and still fail the pull request. Add the flag when
you touch anything concurrent — the daemon, the RPC layer, the store handles,
the TUI's background loads:

```bash
go test -race -count=1 -timeout=120s ./...

cd cmd/graymatter
go test -race -count=1 -timeout=300s ./...
```

`-race` is the one thing here that needs a C compiler, because the detector is
built on CGO. Linux and macOS usually have one already; on Windows install
mingw-w64 or TDM-GCC. Without it the flag fails with `-race requires cgo`, and
the honest fallback is to run the suite without it and let CI catch what you
cannot. That is a gap in your local coverage, not a licence to skip the flag
when you have the toolchain.

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
- **A status surface must check the thing it reports on.** If code answers "is
  this healthy / ready / set up", it has to reach the dependency it is speaking
  for, and it needs a test proving it goes red when that dependency is gone.
  This one is written down because we got it wrong four times in the same
  release: `doctor` reported "everything looks good" for a project that had
  never stored a fact, `/healthz` answered ok with no store and again with a
  dead daemon, and the TUI dashboard drew zeros as though they were data. All
  four were indistinguishable from working, which is worse than an outage: a
  crash gets reported, a confident wrong answer gets believed. Applies equally
  to CI, which reported green for packages it was not running.

---

## Submitting a PR

1. Fork → branch off `main` → make your changes.
2. Add or update tests. The CI will enforce coverage.
3. Run the full test suite locally:
   ```bash
   go test -count=1 -timeout=120s ./... && \
   cd cmd/graymatter && go test -count=1 -timeout=300s ./...
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

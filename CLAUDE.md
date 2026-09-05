# CLAUDE.md

> Read this first if someone pointed you at GrayMatter. Three situations, three
> different answers.

GrayMatter is a single-binary Go memory system for AI agents. Library + CLI +
MCP server + TUI. Facts persist to bbolt with optional vector embeddings, for
roughly a 90% cut in context tokens versus re-injecting full history — the
figure `go run ./benchmarks/token_count` actually prints at 100 sessions.

## A user asked you to set GrayMatter up

Do not read the source for this. Follow **Agent setup procedure** in the
[README](README.md#agent-setup-procedure): install the binary, `graymatter
init`, `graymatter doctor`, then tell the user to restart you.

That last step is the one that gets skipped. MCP servers are launched by the
client at startup, so the memory tools will not exist in the session that ran
`init`, no matter how green `doctor` looks.

## You are working in a project that already uses GrayMatter

[`AGENTS.md`](AGENTS.md) is the operating manual: which tool to call, when, and
with which parameter names. The full version is [`docs/AGENTS.md`](docs/AGENTS.md).

The one thing that reliably trips agents up: `memory_reflect` takes `agent_id`
like the other six tools (`agent` remains only a deprecated alias). Pass at least one; when both are set `agent_id` wins.

## You are contributing to GrayMatter itself

[`CONTRIBUTING.md`](CONTRIBUTING.md) covers setup, tests, coverage gates and
code conventions. Two things the tree does not make obvious:

- **bbolt is single-writer, and daemon mode is what makes concurrent access
  work.** One process owns the store and every other one (TUI, MCP server, CLI,
  `run`, the REST server) connects as a client over a Unix domain socket on
  POSIX, TCP loopback on Windows. On Windows the 256-bit token in the discovery
  file is the *only* access control: any local process can reach loopback, and
  the `0600` the code passes to `os.WriteFile` is a POSIX-only guarantee — on
  Windows the file just inherits its parent directory's ACL.
  Clients spawn the daemon on first use and it idle-exits when unused.
  `--no-daemon` opts out and brings the lock contention back. See
  `cmd/graymatter/internal/daemon/` and `pkg/memory/rpc/`.
- **The module is split.** The root is the library; `cmd/graymatter/` is the CLI
  and TUI with its own `go.mod`, so library consumers do not inherit TUI
  dependencies. [`docs/api-stability.md`](docs/api-stability.md) says what is
  stable: `graymatter.Memory` is, internal packages are not.

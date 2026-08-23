# 002 — bbolt with a single writer, and a daemon to share it

**Status:** Accepted · **Date:** 2026-08-22

## Context

GrayMatter ships as one binary with no service to install and no database to
provision. That constraint picked the storage engine before any comparison of
features: the store has to be a file.

bbolt is a pure-Go embedded B+tree with ACID transactions and a single-file
layout. It takes an **exclusive lock on that file for the life of the
process** — by design, not as a limitation to be worked around.

That collides with how the tool is actually used. A developer runs the TUI in
one terminal, their agent's MCP server holds the store open in the background,
and they type `graymatter recall` in a third window. Three processes, one
file, one lock. The second and third arrivals fail.

## Decision

Keep bbolt, and stop pretending several processes can own it.

**One process owns the store; everyone else is a client.** The first
component that needs the store spawns a daemon, which opens bbolt with
`StrictWrite` and holds the lock. The TUI, the MCP server, the CLI,
`graymatter run` and the REST server all connect to it over a local transport
— a Unix domain socket on POSIX, TCP loopback on Windows — speaking `net/rpc`
from the standard library. The daemon exits when nothing has used it for a
while. `--no-daemon` opts out and accepts the contention.

Two consequences of that shape are worth stating plainly, because they are not
visible from the code:

- **On Windows, the token in the discovery file is the entire access
  control.** Any local process can reach a loopback port. The `0600` passed to
  `os.WriteFile` is a POSIX guarantee that Windows does not honour — there the
  file inherits its parent directory's ACL. See `docs/threat-model.md`.
- **Read-only fallback is a degradation, not a feature.** When the lock cannot
  be acquired and `StrictWrite` is not set, the store opens read-only rather
  than failing. That is right for the TUI and wrong for the daemon, which is
  why the daemon sets `StrictWrite`: a store owner that silently came up
  read-only would break every client attached to it.

## Consequences

- Concurrency is bounded by one writer, always. Writes serialise. For a
  developer-scale store this is invisible; it is a hard ceiling all the same.
- The daemon is a lifecycle problem the project would not otherwise have:
  spawning, idle exit, reconnection, token rotation, orphan detection. Roughly
  the cost of the dependency avoided.
- `net/rpc` is frozen in the standard library and gob-encoded, so the wire
  format is Go-specific and there is no cross-language client. Acceptable —
  the clients all ship in this repo.
- Anything that opens the bbolt file directly, bypassing the daemon, will
  either fail to acquire the lock or corrupt the invariant. `DB()` is an
  escape hatch, not an interface.

## Reversal condition

Move to SQLite (WAL mode, real multi-process concurrency) if any of these
holds:

1. Write contention is measurable in ordinary single-developer use — daemon
   RPC p95 above **50 ms** for `Put` on a store under 10k facts.
2. A supported use case genuinely needs concurrent writers on one machine
   without a coordinating process.
3. Daemon lifecycle bugs — orphans, stale sockets, failed handoffs — account
   for more than **20%** of issues over a release cycle. That would mean the
   coordination cost has exceeded the dependency it was avoiding.

Anything requiring writes from more than one machine is not a reversal of this
decision; see [004](004-local-first-single-node.md).

## Alternatives rejected

- **SQLite.** WAL mode solves the concurrency problem outright, and it is the
  obvious answer if the daemon becomes a liability. It costs cgo, or a pure-Go
  reimplementation with its own risks, against a project whose main claim is a
  single static binary.
- **Postgres.** Correct for a server product. This is not one; see
  [004](004-local-first-single-node.md).
- **Advisory file locks over plain files.** Every hard part of a database,
  reimplemented worse, without transactions.
- **Accept the contention.** What v0.5 did. "Close the TUI before running the
  CLI" is not a usable answer.

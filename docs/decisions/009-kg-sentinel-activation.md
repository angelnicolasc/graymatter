# 009 — KG activation persists as data-dir state (`init --kg` sentinel)

Date: 2026-08-24

## Context

ADR-008 shipped knowledge-graph auto-population gated behind `daemon run --kg`
or `GRAYMATTER_KG=1`. Field reachability was poor by construction:

- MCP clients spawn `graymatter daemon run` themselves with their own
  environment. An exported `GRAYMATTER_KG=1` reaches the daemon only if the
  client itself was launched from that same shell; editors started from a GUI
  never see it.
- Nothing in `init` or `doctor` mentioned the feature, so the opt-in required
  reading the README's CLI table.

The engine-level contract (pkg/memory's wiring-contract tests) was never in
question: `SetKG` stays an explicit call. The question was only where a
user's opt-in lives so every future daemon honours it.

## Decision

`graymatter init --kg` writes an empty marker file, `<data-dir>/kg.auto`.
The daemon resolves activation in exactly one place (`daemon.kgAutoEnabled`)
with OR-semantics: `--kg` flag, or `GRAYMATTER_KG=1` in its own environment,
or the sentinel. Manual runs and client-spawned daemons therefore cannot
disagree.

This is runtime state written by one of our commands, not user-authored
configuration — same statute as `graymatter.http-token`. The "no config
files" product claim is untouched: nothing here asks users to hand-edit
anything, and removal is deleting one file (documented in `init --help`).

Alternatives considered:

- **Flip the default on.** Rejected for now; ADR-008 requires a field cycle,
  and doctor/status did not yet exist to surface field behaviour.
- **Have `init` write the env var into client configs.** Client config files
  are shared surface owned by the tools themselves; injecting environment is
  per-client glue that grows with every supported host.
- **Persist a settings file.** Rejected on principle above; also invites
  becoming a general config surface, which this project deliberately is not.

## Consequences

- Activation survives daemon restarts, machine reboots, and client spawns.
- `doctor` and `status` can read the same marker and report state honestly.
- Turning it off means deleting the file; `--kg=false` semantics were
  deliberately not invented.

## Reversal condition

If a second data-dir behaviour needs persisted opt-in state, promote the
pattern to a single typed key–value store in the data dir (one reader/writer
API) rather than accumulating marker files. If field reports after one
release cycle show auto-population safe and wanted, fold the sentinel into
the default-on decision ADR-008 already conditions on.

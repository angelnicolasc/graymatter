# 004 — Local-first and single-node, deliberately not multi-tenant

**Status:** Accepted · **Date:** 2026-08-22

## Context

"Agent memory" describes two different products. One is infrastructure: a
hosted, multi-tenant service with isolation, quotas, per-tenant encryption and
an availability target. The other is a tool that runs on a developer's machine
and remembers things between sessions.

They share a name and almost no architecture. Nearly every design question in
this repository — storage engine, concurrency model, auth, defaults — resolves
differently depending on which one is being built, so the choice cannot be
deferred.

## Decision

GrayMatter is the second one, and stops there.

One machine, one store, files under `.graymatter/` owned by the user who ran
the command. No accounts, no tenancy, no network by default. The REST and
MCP-HTTP servers bind `127.0.0.1` and require a bearer token; that is a lock on
a local door, not a step toward serving the internet.

This is a scope decision, not a staged plan. Multi-tenancy is not a later
milestone the current design is working toward — the current design is what
falls out of *not* pursuing it.

Concretely, the following are all consequences of this one choice:

- **bbolt over Postgres.** A single-file store with a single writer is only
  viable because the store belongs to one user on one machine
  ([002](002-bbolt-single-writer.md)).
- **No authorisation model.** There is one principal. Anything that can read
  the file can read every fact, and no ACL would change that.
- **Decay tuned for a person.** A 30-day half-life reflects one developer's
  project rhythm ([001](001-decay-half-life.md)), not a workload average
  across tenants.
- **Defaults over configuration.** Anything a single user would not
  reasonably tune does not need to be a setting.

The README says it out loud: *not trying to win the enterprise memory market*.
This record is why that sentence is a design constraint rather than modesty.

## Consequences

- Teams cannot share a memory store. Cross-project federation
  ([#12](https://github.com/angelnicolasc/graymatter/issues/12)) is
  deliberately scoped read-only for the same reason: it stops at reading
  another store, never at writing to a shared one.
- No backup, replication, or point-in-time recovery. The store is a file on
  one disk. `graymatter export` is the recovery story, and it is manual.
- Anyone evaluating this against Mem0 or Zep on tenancy features is comparing
  it against something it does not attempt to be. That is a positioning cost,
  paid deliberately.
- The whole surface stays auditable by one person in an afternoon, which is
  the property the project is actually trading for.

## Reversal condition

This one does not reverse; it forks. If shared multi-writer memory is genuinely
needed, the answer is a separate service with its own storage engine, its own
auth model and its own repository, consuming this library — not a
`--multi-tenant` flag.

The condition for starting that work: more than **five** unrelated users asking
for shared team memory *and* willing to run a server, over a release cycle.
Feature requests alone are not the signal — the willingness to operate a
service is, because that is the actual cost being proposed.

Retrofitting tenancy into this codebase is the failure mode this record exists
to prevent. A single-writer embedded store with no principal model does not
grow into a multi-tenant service; it grows into a multi-tenant service with a
single-writer embedded store at the bottom of it.

## Alternatives rejected

- **Build for both.** The storage engine alone makes this incoherent: bbolt's
  single writer is either an acceptable simplification or a disqualifying
  bottleneck, and it cannot be both.
- **Design for tenancy now, ship single-node.** Pays the full cost of the
  abstraction immediately for a benefit that may never arrive, and an
  abstraction chosen without a real second tenant is usually the wrong one.
- **Stay silent and let people assume.** Costs credibility the moment anyone
  looks, and this project's principal asset is being accurate about itself.

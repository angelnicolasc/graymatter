# ADR 017 — Explicit memory lifecycle and write receipts

Status: Proposed for the integrated candidate.

## Context

Alias observations support query expansion. Feeding them back as ordinary facts makes them visible in context and can incorrectly extend their lifetime. Checkpoint hooks also create lifecycle markers, which can precede a recoverable checkpoint in the same namespace. A caller that retries an uncertain write can create a second fact even if the first write committed. Maintenance used to recheck stored weight without checking recent access and to write one transaction per decayed row.

## Decision

- Apply eligibility by consumer. Context and ordinary recall use live ordinary facts. Alias expansion uses eligible alias rows and records activity only for expansions that contributed to the result.
- Preserve explicit checkpoint loading and listing. The latest resumable selector skips the exact historical marker shapes. Resume distinguishes an empty namespace from corruption and storage errors, and the harness restores conversation messages from the selected checkpoint.
- Return a fact ID from the writer that confirmed the commit. Negotiate a compatible route before dispatch and avoid replay after a lost response. The legacy response can omit the ID when the backend cannot confirm one.
- Require a live row to pass both the stored-weight floor and current-age decay threshold under the pruning transaction. Preserve pinning, retirement, alias rules, and lineage ownership. Apply decay in bounded batches and skip unchanged writes.

## Alternatives and consequences

A global alias filter would also remove valid expansion. Filtering every hook checkpoint by origin would hide useful state. A readback to infer identity is ambiguous for identical text. Weight renewal for all facts would change context priority. A maintenance cooldown without a scheduler would delay extraction and collection without guaranteeing a later run.

The context selector keeps its existing priority. The ordinary weight floor can remain low after recall while the row stays eligible for use. Cancellation can leave a committed maintenance prefix, which the next cycle rereads. Confirmed identities are available for future provenance contracts.

## Verification and reversal

Regression checks cover scoped activity, marker selection and corrupt resume, confirmed identity and single dispatch, current-age pruning, cancellation, reopen and transaction counts. Reconsider context priority only after a separate experiment demonstrates that necessary context survives without displacing durable constraints. Reconsider batch size if a representative local measurement shows reader latency or writer contention exceeds the deployment budget.


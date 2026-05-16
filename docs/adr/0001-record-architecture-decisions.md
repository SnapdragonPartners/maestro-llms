# 0001. Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-05-16

## Context

`maestro-llms` is an app-neutral toolkit with two consumers (Maestro, Morris)
that have deliberately different needs, and a binding spec
(`docs/specification.md`) plus a divergence checklist
(`docs/MAESTRO_DIVERGENCES.md`). Several load-bearing choices have already
been made (e.g. provider packages as leaf imports, the reservation protocol,
capability growth via optional interfaces) and more are coming (the v0.3
middleware set). The spec records *what* must hold; it does not record *why*
a structural path was chosen, what alternatives were rejected, or what
deliberate limitation we are accepting and when to revisit it.

Without that record, a future contributor either relitigates a settled
question or, worse, "fixes" a deliberate limitation without understanding the
tradeoff behind it.

## Decision

Adopt **Architecture Decision Records**. Every significant, hard-to-reverse,
or non-obvious architectural decision gets a numbered ADR in `docs/adr/`,
using the lightweight format described in `docs/adr/README.md`. ADRs are
append-only; a decision changes only by a new ADR that supersedes the old.

ADRs complement, not replace, the existing docs: the spec stays the contract,
the divergences doc stays the cut-over checklist, ADRs carry the rationale.
Retrofitting ADRs for already-made decisions is in scope.

## Consequences

- A small per-decision documentation cost; the payoff is decisions that stay
  decided and limitations that stay understood.
- New significant structural changes are expected to land with an ADR in the
  same PR (mirroring the existing "append a `MAESTRO_DIVERGENCES.md` row"
  convention for Maestro-divergent PRs).
- The first retrofits are ADR-0002 (Ollama no-SDK) and ADR-0003 (middleware
  is `Complete`-only / streaming deferred).

## References

- `docs/specification.md` — binding contract
- `docs/MAESTRO_DIVERGENCES.md` — cut-over divergence checklist
- `CLAUDE.md` — load-bearing structural decisions, kept in sync with ADRs
- Michael Nygard, "Documenting Architecture Decisions" (the format this log follows)

# 0002. Ollama provider: no SDK, hand-rolled `/api/chat` client

- **Status:** Accepted
- **Date:** 2026-05-16 (decision taken with PR #13; retrofitted as an ADR under ADR-0001)

## Context

Maestro's Ollama integration uses the official `github.com/ollama/ollama`
module (its `api` package). Porting that approach directly into `maestro-llms`
would be the path of least resistance and consistent with "adapt Maestro,
don't reinvent."

However, the `ollama` module carries unfixed server-side advisories —
GO-2025-4251, GO-2025-3824, GO-2025-3695 — each listed "Fixed in: N/A".
`govulncheck` attributes these to **any** consumer that imports the module,
regardless of whether the vulnerable server-side code paths are reached.
`maestro-llms` runs `govulncheck` as a security gate, so importing the SDK
fails the build for both consumers.

The Ollama surface we actually need is a single, stable JSON endpoint
(`POST /api/chat`). The SDK's value-add over a direct call is minimal here,
and a non-goal of this package is to minimize transitive dependencies.

## Decision

Do **not** import the `ollama` module. Implement
`llms/providers/ollama` as a minimal hand-rolled `net/http` client speaking
the `/api/chat` JSON contract directly, classifying errors through the shared
`apierr` helper like the other providers.

## Consequences

- The security gate passes; no unfixable advisory is dragged into either
  consumer. The minimal-dependency non-goal is satisfied.
- We now **own** the `/api/chat` wire contract: schema drift across Ollama
  server versions is our problem to track, not the SDK's. Behaviour parity
  with Maestro (chat + tool-use round trip) was verified live before the port
  was accepted.
- Bonus fidelity: real HTTP status/headers and raw tool-call argument bytes,
  rather than SDK-normalized shapes — consistent with the other providers'
  typed `apierr` classification.
- This is also tracked as divergence **OL1** in
  `docs/MAESTRO_DIVERGENCES.md`; that row is the cut-over checklist entry,
  this ADR is the rationale of record.

## References

- `docs/MAESTRO_DIVERGENCES.md` — row OL1 (and OL2–OL4 for related Ollama deltas)
- `llms/providers/ollama/` — the hand-rolled client
- `llms/providers/internal/apierr/` — shared error classifier
- PR #13 "v0.2: Ollama chat — no SDK dependency (final port)"
- Advisories: GO-2025-4251, GO-2025-3824, GO-2025-3695

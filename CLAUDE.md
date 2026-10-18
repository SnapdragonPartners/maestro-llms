# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) and other coding agents (Codex reads it as AGENTS.md) when working with code in this repository. AGENTS.md is a symlink to this file.

## Current state

**v0.12.0** is the current line. Shipped: core chat/embedding interfaces, errors, fakes, the reservation limiter + in-memory limiter; five provider chat adapters (Anthropic, OpenAI Responses, Google, Ollama-no-SDK, vLLM via OpenAI Chat Completions) + OpenAI and Gemini/Vertex embeddings; the full provider-neutral middleware set (`Validation`, `Retry`, `Timeout`, `Circuit`, `RateLimit`, `Metrics`) with `ChainChat`/`ChainEmbeddings` and `Recommended*`; **v0.4 (ADR-0009)** Anthropic-on-Vertex (`anthropicvertex` leaf + `anthropic.WithRequestOptions`), Gemini/Vertex embeddings, task-typed embeddings, app-supplied auth + PSC injection (no ADC), fail-closed `MaxInputBytes`; **v0.4.1/0.4.2** OpenAI `StopReason` (OC4) and Gemini `thought_signature` round-trip (ADR-0010); **v0.5 (ADR-0011)** `llms/toolloop`; **v0.6** `ModelLister`/`LatestInFamily` (ADR-0012) + `EstimateTextTokens` (ADR-0013); **v0.7** vLLM provider (ADR-0015), **v0.7.1** `Usage.ReasoningTokens`/`BillableOutputTokens` (ADR-0016); **v0.8 (ADR-0017)** toolloop terminal tools (`ToolResult.Stop` → `OutcomeTerminalTool`, return-statement semantics within a turn) and the `MaxIterations` limit turn is now executed; anthropic-sdk-go v1.82 / genai v1.75, and `anthropic.New` uses `option.WithoutEnvironmentDefaults()` (no implicit `ANTHROPIC_*` env sourcing, divergence A6); **v0.8.1** `openai`/`vllm` likewise ignore `OPENAI_*` env (divergence X6, clients built without `openai.NewClient`) and `vllm` is the generic OpenAI-compatible client, verified on LunaRoute (base URL includes `/v1`); **v0.9 (ADR-0018)** openai-go v3, Anthropic `ReasoningTokens` from `output_tokens_details.thinking_tokens` (presence-checked), toolloop prealloc capped; **v0.10** toolloop `OutcomeDeadline` + `NoIterationLimit` (ADR-0019, `classifyStop` is the choke point for context-driven stops) and `OutcomeEmptyResponse` (ADR-0020, empty response not appended), adapters omit `tool_choice` with no tools (#88, divergence X7); **v0.11** Anthropic thinking blocks round-trip as `ContentReasoning` parts with a typed `llms.ProviderID` (ADR-0021; `ReasoningFrom` is the per-adapter choke point, `WithoutReasoning` before editing history), provider-neutral `ChatRequest.Reasoning` + `ChatResponse.AppliedReasoning` (ADR-0022; 1:1 mapping or omitted, no per-model prediction), `Usage.InputTokens` includes cached input with cache reads/writes as subsets (ADR-0023, Anthropic behavior change, divergence X9), and per-request `ChatRequest.ProviderParams` deep-merged by `llms/providers/internal/extraparams` (ADR-0024; testllm records them as JSON); **v0.12 (ADR-0025)** the generic top-level `bucket` limiter (no `llms` import; `Acquire(ctx, key, tokens)` → `Lease{Adjust, Release, Lost}`) with `bucket/memory` and `bucket/postgres` (DB-owned limits via `Ensure`, renewed lease rows, `clock_timestamp()` after the row lock, `MaxWait` a hard deadline, versioned migrations in a dedicated `bucket` schema with `SchemaError` fail-fast; Postgres tests gated on `BUCKET_POSTGRES_DSN`, required in CI), adapted by `ratelimit.FromBucket`. Live coverage: per-provider `integration_test.go` + shared `llms/internal/livetest` toolloop scenarios + Vertex tests (gated on `VERTEX_PROJECT`) + LunaRoute tests (gated on `LUNAROUTE_API_KEY`; per-key concurrency limit 2, run with `-test.parallel=1`). Dependabot is enabled; root bumps need `make tidy` to re-sync `examples/*` (CI checks drift). Standard Go toolchain (`go.mod`; `make build|test|lint|tidy`). Streaming is still deferred per ADR-0003.

`docs/specification.md` is the binding contract for v0. Read it before writing code. The "Normative Clarifications From Engineering Review" and "Resolved by review" sections are decisions, not suggestions — do not relitigate them in code (e.g. tool calls/results are `ContentPart`s not side-channel fields; `System` is a dedicated `ChatRequest` field; `Temperature` is `*float32`; streaming is a separate `StreamingChatClient` interface; local limiter rejections use `LimitError`, not `ProviderError`).

`docs/adr/` is the **Architecture Decision Record** log (see `docs/adr/README.md`): the rationale of record for significant structural choices, with the spec as the contract and `docs/MAESTRO_DIVERGENCES.md` as the cut-over checklist. Read relevant ADRs before changing a documented decision; do not "fix" a deliberate limitation an ADR explains. Significant structural changes should land an ADR in the same PR. Notably ADR-0003: middleware wraps `Complete`/`Embed` only — streaming-aware middleware semantics are deliberately deferred until a real consumer exists, so do not add streaming forwarding to middleware without a superseding ADR.

## What this package is

`maestro-llms` is an app-neutral Go toolkit wrapping LLM/embedding providers behind stable interfaces, shared by three consumers with deliberately different needs:

- **Maestro** — desktop/local, in-process rate limiting.
- **Morris** — cloud (Cloud Run, multi-instance), needs shared/distributed rate limiting, audit, content classification.
- **maestro-cms** — content service that does budget-aware text chunking for embeddings (requester of ADR-0013's `EstimateTextTokens`).

The core design tension to keep in mind on every change: **nothing in this package may import product-specific assumptions from any consumer.** Config resolution, secret sourcing, audit taxonomy, distributed limiter storage, content rules, and chunking policy all belong to the applications, not here. When a feature seems to need app context, the answer is almost always an interface the app implements or a callback hook — not a concrete implementation in this package.

## Architecture (planned, per spec)

Planned module `github.com/SnapdragonPartners/maestro-llms`, core import name `llms`.

```
llms/                core interfaces and shared types (ChatClient, EmbeddingClient, Message, errors)
llms/middleware/     provider-neutral middleware (timeout, retry, circuit, ratelimit, metrics, validation)
llms/ratelimit/      Limiter/Reservation interfaces + optional in-memory limiter
llms/providers/{anthropic,openai,google,ollama,vllm}/   one package per provider, imported only if used
llms/testllm/        deterministic FakeChatClient / FakeEmbeddingClient
bucket/              generic per-key token/concurrency limiter (no llms import); bucket/memory, bucket/postgres (ADR-0025)
```

Load-bearing structural decisions:

- **Provider packages are leaf imports.** The core `llms` package must not pull provider SDKs. Apps import only the provider packages they use.
- **One app-neutral conversation model.** `Message`/`ContentPart` is the single representation; each provider adapter translates it to/from that provider's wire shape at the provider boundary (e.g. Anthropic encodes `RoleTool` results as user-role content blocks; OpenAI uses tool-role messages). The Maestro implementation is the tested reference for the four ported providers (Anthropic, OpenAI Responses, Google, Ollama) — adapt it, don't reinvent provider behavior. vLLM (ADR-0015) is greenfield: no Maestro reference, designed fresh against the OpenAI Chat Completions wire shape via openai-go with a configurable base URL.
- **Middleware is `func(Client) Client`, composed via `ChainChat`/`ChainEmbeddings`; first argument is outermost.** Composition order is semantically significant (see the spec's recommended order and the retry-vs-reservation tradeoff). Changing order changes correctness, not just performance.
- **Rate limiting is a reservation protocol** (`Reserve` → `Commit` actuals → always `Release`), not a concrete limiter. The in-memory limiter ships here; the distributed (Postgres) one is implemented in Morris first and only promoted here if it stays app-neutral. `Release` must run on a context that survives request cancellation (`context.WithoutCancel`).
- **Capability growth via optional interfaces + type assertion**, never by widening core interfaces. This applies to `StreamingChatClient` and `LimiterStats`. Adding a method to `ChatClient` or `Limiter` is a breaking change for every provider, fake, and middleware — treat it as such.
- **Deterministic fakes are first-class**, not an afterthought: both consumers run most tests without real provider calls, so `testllm` correctness gates everything downstream.

## Build/test commands

`make build` (lint + `go build ./...`), `make test` (unit tests w/ coverage; single: `make test TESTARGS='-run TestName ./llms/...'`), `make lint` (gofmt + golangci-lint — the lint gate is strict: `fieldalignment`, `gocritic rangeValCopy`, `revive` unused-params, modernize `min`/`max`/`WaitGroup.Go` all fail CI). Live provider tests: `make test-integration` (OS-aware; see README). CI requires `build-lint-test` + `CodeQL` (aggregate only — see memory).

## Versioning

Pre-1.0; v0.x minor versions may break. Shipped lines: see README "Versioning & license" (v0.1.0 through v0.12.0). Each PR that intentionally differs from Maestro appends a `docs/MAESTRO_DIVERGENCES.md` row; significant structural decisions land an ADR (`docs/adr/`) in the same PR. **Any release that raises `bucket/postgres.SchemaVersion` must open its release notes with a "Database migration required" section** (the new version, the migration files, the rollout order "migrate, then deploy", and the `SchemaError` an upgrader sees if they skip it), and flag it in the release title (ADR-0025): unit tests pass without the migration, so it is easy to miss. Don't expand a release's scope mid-stream — scope is decided up front and tracked in the session plan.

## Branch and review workflow

Several consumers (Maestro, Morris, maestro-cms, buggrit) depend on this package, so substantive work follows a **review-before-push** loop (the same process as Morris): the heavy review happens on the local branch *before* the push. Post-push checks (CI, CodeQL, the automatic Copilot review) are a lighter final proofread.

**Roles:**

- **Claude** authors: code, tests, ADRs, docs, and fixes.
- **Codex** is the primary reviewer. It reviews the committed local branch **before push** through the **Counterpoint** tool (`mcp__counterpoint__review`). If the tool is not available in the session, write the branch notes and ask Dan to relay them to Codex; do not ask Dan to relay when the tool is available.
- **Dan** orchestrates, resolves disagreements, authorizes pushes, and merges. **Claude never merges.**
- A PR is **accepted** only after both Codex and Dan approve.

**Steps:**

1. Branch off an up-to-date `main` (`feat/<slug>`, `fix/<slug>`, `docs/<slug>`, `chore/<slug>`, `test/<slug>`). Never work directly on `main`.
2. Make the scoped change. Verify in proportion to risk: `make build` (lint + build) and `make test` always; `govulncheck ./...` and `make tidy` when dependencies change; the live tests for every provider the change touches (`scripts/integration-local.sh -test.run '<regex>'` on macOS; keys and gating per README). Do not punt live tests to CI: the integration workflow is manual-only.
3. Commit **locally**. The pre-push hook (`hooks/pre-push`: lint + unit tests) must pass; never bypass it with `--no-verify`.
4. Write branch notes (below) and submit the exact local commit through Counterpoint (below).
5. Address every blocking finding with a fix or a reasoned response. Commit each review round locally and resubmit with updated notes.
6. Continue until Codex has no blocking findings, Dan explicitly overrides one, or a disputed finding has been escalated to Dan.
7. **Stop at the push gate.** Quote Codex's approval (or remaining findings) to Dan. **Do not push or open a PR until Codex and Dan have both approved**; push is a human gate, not a routine step. Approval covers that push only.
8. After approval, push and open the PR per *PR body conventions*. Treat post-push findings substantively (below).
9. Dan gives final approval and merges. After a merge, close any issues the PR resolves and update the session status memory.

**One branch at a time.** Keep at most **one feature/development branch open** (pushed or not) at a time. Land it (review → push → merge) before starting the next. No stacked or dependent PRs. Exceptions: urgent bug fixes, Dependabot PRs, or parallel work Dan explicitly authorizes. When a release spans several issues, they still land one PR at a time, in sequence. A docs-only companion may ride along with its code.

### Responding to findings

Codex is a reviewer, not an authority. Only **blocking** findings must be resolved before the push gate:

- **P0**: wrong results or corruption, a security exposure (for example credentials sent to the wrong endpoint), or an unusable API.
- **P1**: a concrete correctness, robustness, compatibility, or maintainability defect, including an undocumented breaking change for consumers or a missing ADR / `MAESTRO_DIVERGENCES.md` row this file requires.

Suggestions and style points (P2 and below) are at Claude's discretion; record their disposition in the next round's notes. A finding Claude judges wrong, out of scope, or disproportionate gets a reasoned response rather than a change: cite the spec, an ADR, an existing test, or the deliberate design choice. When pushback is correct on the merits, add a regression test alongside it. If Codex reaffirms a disputed finding, **stop**, state both positions, and bring the decision to Dan before another round. Do not implement a change Claude believes is wrong in order to end the loop.

**A repeated finding is a class.** When two or three findings, from any reviewer, share one shape (the same boundary, the same missing check), stop patching instances: name the class, fix it at one choke point with a test per case, say so in the branch notes, and ask Codex to enumerate every input at that boundary rather than reviewing only the delta.

### Submitting for review

- Call `mcp__counterpoint__review` with the absolute repository path (from `git rev-parse --show-toplevel`; never hard-coded), the branch, the exact local commit, and the branch notes. The commit must be the branch tip and the checked-out HEAD of a **clean** worktree, so commit first. The call blocks for the whole Codex turn (several minutes, up to ~21).
- In a **read-only** review (default) the reviewer reads the worktree, so **do not touch the worktree while it runs**. In a **build-capable** review (`build: true`) the reviewer gets a disposable, **offline** checkout; just don't rewrite the branch meanwhile.
- Ask for a build-capable review when a test run is material evidence: code changes, especially to provider wire conversion, error classification, middleware ordering/concurrency, the limiter, or `toolloop`. Keep docs-only rounds and small follow-ups read-only. The offline checkout cannot run live provider tests (they need keys and network), so their commands and outcomes are the author's evidence and must be in the notes.
- The reviewer starts each round fresh; its memory is the last three rounds' verdicts. Write later rounds' notes as a delta but **keep the disposition of every prior finding**, since older rounds are evicted. A new or renamed branch starts a new review thread.
- If the tool reports the thread is unavailable, stop and tell Dan; the error says what to do.

### Branch notes

The handoff artifact, not a generic summary. Codex verifies claims against the commit and repository, so verification claims must be backed by commands and outcomes. Every round's notes **lead with the exact commit hash and the push state** (`not pushed`), then include:

- review round number, and whether the change is code, docs, or both;
- verification commands and outcomes (`make build`, `make test`, `govulncheck`, live tests with the env/model used);
- material changes and why; for multi-issue releases, which issue this branch closes and what it deliberately does NOT do;
- design references (spec sections, ADRs) with the controlling sentence for any non-obvious decision; the ADR and `MAESTRO_DIVERGENCES.md` rows this change adds, or why none is needed;
- consumer impact: anything a downstream app must change, for the release notes;
- resolution of each prior finding (fixed in commit X / reasoned pushback / deferred with reason);
- design choices and rejected alternatives; known limitations and open questions.

The same notes seed the PR body once push is approved.

### PR body conventions

AI reviewers (Copilot) see only the diff and the PR body, so the body must carry the design context:

1. **Summary**: what changed and why, in plain language.
2. **Design references**: links to the relevant ADRs / spec sections, quoting the controlling sentence for any amendment or non-obvious decision.
3. **Context**: the issue it closes, related PRs, and what this PR does NOT yet do, so intentional gaps aren't flagged as bugs.
4. **Consumer impact**: breaking or behavioral changes, for the release notes.
5. **Test plan**: `make build`, `make test`, live-test commands and outcomes.

End the body with the Claude Code attribution line; end commits with the `Co-Authored-By` trailer.

### Post-push reviewer feedback

Apply the same standard as *Responding to findings*. Fix real bugs in a dedicated commit ("Address PR #N review feedback"). Pushing a fix is another push: a small fix for a post-push finding needs no fresh Codex round unless it changes design or behavior, but a substantive change goes back through Counterpoint and the push gate. **Then reply on each thread citing the fixing commit and resolve the thread** (GraphQL `resolveReviewThread`); a summary comment is not enough. Check again for new feedback after each push.

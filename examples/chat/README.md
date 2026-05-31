# examples/chat — runs-locally web chat demo

A small reference web app that exercises every provider in `maestro-llms`
through one UI. It:

- Reads provider credentials from the environment.
- Picks one model per reachable provider — latest in each provider's
  preferred family for the hosted ones; the most-recently-pulled local
  model for Ollama; the first served model for vLLM.
- Serves a single-page chat UI on `http://localhost:8765` with a dropdown
  populated from those models.
- Wires each completion through `middleware.RecommendedChat` so the demo
  reflects the production-shape wiring a real service would use.

This module lives under its own `go.mod` so its dependency closure does
not leak into the main toolkit's. Replace the `replace` directive in
`go.mod` with a real version pin when copying this out of the monorepo.

## Run

```sh
cd examples/chat

# Set whichever provider credentials you have:
export ANTHROPIC_API_KEY=sk-ant-...      # or MAESTRO_ANTHROPIC_API_KEY
export OPENAI_API_KEY=sk-...
export GEMINI_API_KEY=...                # or GOOGLE_GENAI_API_KEY / GOOGLE_API_KEY
export OLLAMA_HOST=http://localhost:11434  # defaults to localhost:11434 if Ollama is running
export MAESTRO_VLLM=http://my-vllm:8000  # full base URL of a vLLM instance

go run .
# open http://localhost:8765
```

Override the port with `-addr :<port>` or the `PORT` env var.

If no provider credentials are set, the server prints a clear error
listing the env vars it looks for and exits 1.

## What to read

- `providers.go` — env detection + model picking. One function per
  provider, all following the same shape: env check → construct client →
  `ListModels` (under a per-provider deadline) → pick a model. The
  hosted providers iterate a small preferred-family list and surface
  the newest in the first family that has any models. Ollama picks the
  most-recently-pulled local model (the developer's likely working
  set); vLLM takes the first model the server reports (vLLM usually
  serves exactly one).
- `server.go` — HTTP handlers and the per-request `Complete` wiring. The
  important block is the `middleware.RecommendedChat(...)` call: that is
  exactly what a real service would do.
- `ui/app.js` — single-file SPA. Includes a small client-side token
  estimator that approximates `llms.EstimateTextTokens` for the
  pre-send hint.

## Stack

- Go stdlib (`net/http`, `embed`) — no new toolkit-side deps.
- maestro-llms providers, middleware, and helpers.
- ~1,000 lines total, plain HTML/CSS/JS frontend.

## What it does NOT show

- **Streaming.** Deferred per ADR-0003; the demo uses synchronous
  `Complete` only.
- **Tool calls.** Possible follow-up: wire in `llms/toolloop` with a
  sample tool. Skipped for the v1 of this demo to keep the wiring
  approachable.
- **Persistence.** Conversation state is in-memory on the page and
  resets on reload.
- **Auth / multi-user.** Single-user local demo; no per-user state.

## Pinning

This module imports the toolkit via a local `replace` directive so it
always uses your worktree copy. If you copy this demo elsewhere, drop
the `replace` and add a real version pin (`go get
github.com/SnapdragonPartners/maestro-llms@v0.7.0` or whatever is
current).

# examples/otel — exporting to OpenTelemetry

Runs a small tool loop (one `clock` tool) and exports its traces and metrics
over OTLP/HTTP with the `llmotel` adapter
([ADR-0051](../../docs/adr/0051-otel-module.md)). By default the model is a
scripted fake, so no provider key is needed.

This module has its own `go.mod` with `replace` directives onto the parent
worktree. When you copy it out of the monorepo, replace them with pinned
versions.

## Run

Any OTLP/HTTP collector works. The quickest is Grafana's all-in-one image:

```sh
docker run --rm -p 3000:3000 -p 4317:4317 -p 4318:4318 grafana/otel-lgtm
```

Then:

```sh
cd examples/otel
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 go run .
# outcome: final_answer after 1 tool-calling turn(s)
# trace: <trace id>
```

Against a live provider:

```sh
LLM_API_KEY=... OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
  go run . -provider anthropic -model claude-sonnet-5-5
```

(`ollama` needs no key; `openaicompat` also needs `-base-url`, e.g.
`http://host:8000/v1`; Vertex isn't wired in this example.)

## What to look at

In Grafana (`http://localhost:3000`), **Explore → Tempo**, search for the
printed trace ID. The tree is:

```
example request
└── invoke_agent otel-example                 gen_ai.operation.name=invoke_agent, outcome, token totals
    ├── maestro_llms.call chat <model>        one per logical call (span only)
    │   └── chat <model>                      one per provider attempt: usage, finish reason, attempt
    ├── execute_tool clock                    gen_ai.tool.name, gen_ai.tool.call.id
    └── maestro_llms.call chat <model>
        └── chat <model>
```

**Explore → Prometheus**: `gen_ai_client_operation_duration_seconds_*` and
`gen_ai_client_token_usage_*`, labeled by operation, provider and model (and
`gen_ai_token_type` for tokens).

No prompt, completion or tool argument is exported, and neither is error
text: a failed call shows only `error.type`.

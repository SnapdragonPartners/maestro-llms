# otel — OpenTelemetry for maestro-llms

A separate v0 Go module, `github.com/SnapdragonPartners/maestro-llms/otel`,
tagged `otel/vX.Y.Z` and outside the root module's v1 stability contract.
Its one package, `llmotel`, exports chat, embedding, tool and tool-loop calls
as OpenTelemetry GenAI spans, and chat and embedding calls as duration and
token-usage metrics. Design and attribute mapping:
[ADR-0051](../docs/adr/0051-otel-module.md). Usage: the root README's
"OpenTelemetry" section and [`examples/otel`](../examples/otel).

The package depends only on the OpenTelemetry API. The SDK and OTLP
exporters in `go.mod` are used by the tests.

## Tests

```sh
make test-otel            # unit tests (in-memory span recorder, manual metric reader)
```

The live test exports one traced tool-loop run over OTLP/HTTP and reads it
back. It is build-tagged `integration` and skips unless both required
variables are set:

| Variable | Meaning |
|---|---|
| `LLMOTEL_LIVE_OTLP_ENDPOINT` | OTLP/HTTP base URL, e.g. `http://host:4318` |
| `LLMOTEL_LIVE_TEMPO_URL` | Tempo HTTP API base; the trace is read from `{base}/api/traces/{id}` |
| `LLMOTEL_LIVE_PROM_URL` | optional Prometheus HTTP API base; when set, the duration count and token sums are checked too |

Against Grafana's all-in-one `grafana/otel-lgtm` image, the Tempo and
Prometheus bases can be Grafana's datasource proxies:

```sh
cd otel
go test -c -tags=integration -o /tmp/llmotel.test ./llmotel
codesign -s - -f /tmp/llmotel.test   # macOS only: unsigned test binaries hang in dyld
LLMOTEL_LIVE_OTLP_ENDPOINT=http://host:4318 \
LLMOTEL_LIVE_TEMPO_URL=http://host:3000/api/datasources/proxy/uid/tempo \
LLMOTEL_LIVE_PROM_URL=http://host:3000/api/datasources/proxy/uid/prometheus \
  /tmp/llmotel.test -test.run Integration -test.v
```

On Linux, `go test -tags=integration -run Integration -v ./llmotel` works
directly.

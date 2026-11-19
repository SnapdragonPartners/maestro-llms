//go:build integration

package llmotel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/otel/llmotel"
)

// TestIntegrationExportToCollector exports one traced tool-loop run over
// OTLP/HTTP and reads it back (ADR-0051 §6).
//
//	LLMOTEL_LIVE_OTLP_ENDPOINT  OTLP/HTTP base URL, e.g. http://host:4318
//	LLMOTEL_LIVE_TEMPO_URL      Tempo HTTP API base; the trace is read from
//	                            {base}/api/traces/{id}
//	LLMOTEL_LIVE_PROM_URL       optional Prometheus HTTP API base; when set,
//	                            the duration metric is queried from
//	                            {base}/api/v1/query
//
// Against Grafana's LGTM image, the Tempo and Prometheus bases can be
// Grafana's datasource proxies, e.g.
// http://host:3000/api/datasources/proxy/uid/tempo.
func TestIntegrationExportToCollector(t *testing.T) {
	endpoint := os.Getenv("LLMOTEL_LIVE_OTLP_ENDPOINT")
	tempo := strings.TrimRight(os.Getenv("LLMOTEL_LIVE_TEMPO_URL"), "/")
	if endpoint == "" || tempo == "" {
		t.Skip("LLMOTEL_LIVE_OTLP_ENDPOINT and LLMOTEL_LIVE_TEMPO_URL not set")
	}
	prom := strings.TrimRight(os.Getenv("LLMOTEL_LIVE_PROM_URL"), "/")
	ctx := context.Background()

	res := resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("llmotel-integration"))
	texp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(strings.TrimRight(endpoint, "/")+"/v1/traces"))
	if err != nil {
		t.Fatal(err)
	}
	mexp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(strings.TrimRight(endpoint, "/")+"/v1/metrics"))
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(texp), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)), sdkmetric.WithResource(res))

	tel, err := llmotel.New(llmotel.Options{TracerProvider: tp, MeterProvider: mp})
	if err != nil {
		t.Fatal(err)
	}
	// A unique model name scopes the read-back to this run.
	model := fmt.Sprintf("llmotel-live-%d", time.Now().UnixNano())
	tc := testllm.ToolCallResponse("call-1", "echo", `{}`)
	tc.Usage = llms.Usage{InputTokens: 11, BillableOutputTokens: 2}
	final := testllm.TextResponse("done")
	final.Usage = llms.Usage{InputTokens: 17, BillableOutputTokens: 3}
	fake := &testllm.FakeChatClient{
		ModelRef:  llms.ModelRef{Provider: llms.ProviderAnthropic, Name: model},
		Responses: []llms.ChatResponse{tc, final},
	}

	ctx, root := tp.Tracer("llmotel-integration").Start(ctx, "integration root")
	traceID := root.SpanContext().TraceID().String()
	out := tel.Run(ctx, toolloop.Config{
		Request: llms.ChatRequest{Messages: []llms.Message{llms.UserText("go")}},
		Client:  tel.ChatCall()(tel.Chat()(fake)),
		Tools:   tel.Tools([]toolloop.Tool{echoTool(false, nil)}),
	}, llmotel.RunOptions{AgentName: "integration"})
	root.End()
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("outcome = %s (%v)", out.Kind, out.Err)
	}
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("trace export: %v", err)
	}
	if err := mp.Shutdown(ctx); err != nil {
		t.Fatalf("metric export: %v", err)
	}
	t.Logf("trace %s, model %s", traceID, model)

	want := []string{
		"integration root", "invoke_agent integration", "execute_tool echo",
		"maestro_llms.call chat " + model, "chat " + model,
	}
	body := poll(t, tempo+"/api/traces/"+traceID, func(b string) bool {
		for _, w := range want {
			if !strings.Contains(b, `"`+w+`"`) {
				return false
			}
		}
		return true
	})
	for _, attr := range []string{"gen_ai.usage.input_tokens", "gen_ai.tool.call.id", "maestro_llms.toolloop.outcome"} {
		if !strings.Contains(body, attr) {
			t.Errorf("trace lacks %s", attr)
		}
	}

	if prom == "" {
		return
	}
	// Two chat attempts: two duration records, and token sums equal to the
	// two scripted usages (11+17 input, 2+3 output). Metric names follow the
	// OTLP-to-Prometheus translation (unit suffix, dots to underscores).
	for query, want := range map[string]string{
		fmt.Sprintf(`sum({__name__=~"gen_ai_client_operation_duration(_seconds)?_count", gen_ai_request_model=%q})`, model):                   "2",
		fmt.Sprintf(`sum({__name__=~"gen_ai_client_token_usage(_tokens)?_sum", gen_ai_request_model=%q, gen_ai_token_type="input"})`, model):  "28",
		fmt.Sprintf(`sum({__name__=~"gen_ai_client_token_usage(_tokens)?_sum", gen_ai_request_model=%q, gen_ai_token_type="output"})`, model): "5",
	} {
		poll(t, prom+"/api/v1/query?"+url.Values{"query": {query}}.Encode(), func(b string) bool {
			var r struct {
				Data struct {
					Result []struct {
						Value []any `json:"value"`
					} `json:"result"`
				} `json:"data"`
			}
			return json.Unmarshal([]byte(b), &r) == nil && len(r.Data.Result) == 1 &&
				len(r.Data.Result[0].Value) == 2 && r.Data.Result[0].Value[1] == want
		})
	}
}

// poll GETs u until ok accepts the body or 60s pass.
func poll(t *testing.T, u string, ok func(string) bool) string {
	t.Helper()
	// The deadline bounds each request, headers and body read included, so
	// a stalled collector fails here instead of at the go test timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var last string
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
		if err != nil {
			t.Fatalf("request %s: %v", u, err)
		}
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // test URL from the environment
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			last = fmt.Sprintf("%d %s", resp.StatusCode, b)
			if resp.StatusCode == http.StatusOK && ok(string(b)) {
				return string(b)
			}
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatalf("%s: not found within 60s; last response: %.500s", u, last)
	return ""
}

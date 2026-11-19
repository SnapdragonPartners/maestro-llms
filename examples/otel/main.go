// Command otel runs a small tool loop and exports its spans and metrics to an
// OpenTelemetry collector over OTLP/HTTP, using the llmotel adapter
// (ADR-0051). See README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/otel/llmotel"
)

func main() {
	provider := flag.String("provider", "", "provider ID (anthropic, openai, google, ollama, openaicompat); empty runs a scripted fake")
	model := flag.String("model", "", "model name for -provider")
	prompt := flag.String("prompt", "What time is it in UTC? Use the clock tool.", "user prompt")
	flag.Parse()

	if err := run(*provider, *model, *prompt); err != nil {
		log.Fatal(err)
	}
}

func run(provider, model, prompt string) (err error) {
	ctx := context.Background()

	// The application owns the SDK. The OTLP exporters read the standard
	// OTEL_EXPORTER_OTLP_ENDPOINT (e.g. http://localhost:4318) and related
	// variables; llmotel itself reads no environment.
	texp, err := otlptracehttp.New(ctx)
	if err != nil {
		return fmt.Errorf("trace exporter: %w", err)
	}
	mexp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return fmt.Errorf("metric exporter: %w", err)
	}
	res := resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("maestro-llms-otel-example"))
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(texp), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)), sdkmetric.WithResource(res))
	defer func() {
		// Shutdown flushes the batched spans and the last metric collection.
		err = errors.Join(err, tp.Shutdown(ctx), mp.Shutdown(ctx))
	}()

	tel, err := llmotel.New(llmotel.Options{TracerProvider: tp, MeterProvider: mp})
	if err != nil {
		return err
	}

	base, err := client(provider, model)
	if err != nil {
		return err
	}
	// Attempt layer innermost (one span per provider attempt, plus metrics),
	// logical-call layer outermost (span only), so attempts nest under it.
	chat := tel.ChatCall()(middleware.RecommendedChat(tel.Chat()(base), middleware.RecommendedConfig{}))

	ctx, root := tp.Tracer("otel-example").Start(ctx, "example request")
	out := tel.Run(ctx, toolloop.Config{
		Request: llms.ChatRequest{Messages: []llms.Message{llms.UserText(prompt)}, MaxTokens: 1024},
		Client:  chat,
		Tools:   tel.Tools([]toolloop.Tool{clockTool()}),
	}, llmotel.RunOptions{AgentName: "otel-example"})
	root.End()

	fmt.Printf("outcome: %s after %d tool-calling turn(s)\n", out.Kind, out.Iterations)
	if out.Err != nil {
		fmt.Printf("error: %v\n", out.Err)
	}
	if out.Response.Text != "" {
		fmt.Printf("answer: %s\n", out.Response.Text)
	}
	fmt.Printf("trace: %s\n", root.SpanContext().TraceID())
	return nil
}

// client returns a live provider client, or a scripted fake that calls the
// clock tool once and then answers.
func client(provider, model string) (llms.ChatClient, error) {
	if provider == "" {
		call := testllm.ToolCallResponse("call-1", "clock", `{}`)
		call.Usage = llms.Usage{InputTokens: 120, OutputTokens: 12, BillableOutputTokens: 12}
		answer := testllm.TextResponse("It is a fine time in UTC.")
		answer.Usage = llms.Usage{InputTokens: 160, OutputTokens: 9, BillableOutputTokens: 9}
		return &testllm.FakeChatClient{
			ModelRef:  llms.ModelRef{Provider: llms.ProviderAnthropic, Name: "scripted-fake"},
			Responses: []llms.ChatResponse{call, answer},
		}, nil
	}
	if model == "" {
		return nil, errors.New("-model is required with -provider")
	}
	c, err := providers.New(llms.ProviderID(provider), providers.Options{
		Model:  model,
		APIKey: os.Getenv("LLM_API_KEY"),
	})
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}
	return c, nil
}

func clockTool() toolloop.Tool {
	return toolloop.Tool{
		Definition: llms.ToolDefinition{
			Name:        "clock",
			Description: "Returns the current time in UTC (RFC 3339).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: time.Now().UTC().Format(time.RFC3339)}, nil
		},
	}
}

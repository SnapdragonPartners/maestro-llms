//go:build integration

package anthropicvertex_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic/anthropicvertex"
)

// Live Anthropic-on-Vertex tests. Skipped unless VERTEX_PROJECT is set.
//
// The package does no credential discovery (ADR-0009). This test plays the
// app: it loads Application Default Credentials itself (locally:
// `gcloud auth application-default login`) and passes them in explicitly.
//
//	VERTEX_PROJECT          GCP project with Vertex AI + Claude enabled (required)
//	VERTEX_REGION           default "global"
//	VERTEX_ANTHROPIC_MODEL  default "claude-opus-4-6"
func newVertexClient(t *testing.T) llms.ChatClient {
	t.Helper()
	project := os.Getenv("VERTEX_PROJECT")
	if project == "" {
		t.Skip("VERTEX_PROJECT not set")
	}
	region := envOr("VERTEX_REGION", "global")
	model := envOr("VERTEX_ANTHROPIC_MODEL", "claude-opus-4-6")
	ctx := context.Background()
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err != nil {
		t.Skipf("no Google credentials (run `gcloud auth application-default login`): %v", err)
	}
	c, err := anthropicvertex.New(
		anthropicvertex.WithContext(ctx),
		anthropicvertex.WithProjectID(project),
		anthropicvertex.WithRegion(region),
		anthropicvertex.WithModel(model),
		anthropicvertex.WithCredentials(creds),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Logf("Anthropic on Vertex: project=%s region=%s model=%s", project, region, model)
	return c
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func TestIntegrationVertexAnthropicChat(t *testing.T) {
	c := newVertexClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.Complete(ctx, llms.ChatRequest{
		Purpose:   llms.PurposeChat,
		Messages:  []llms.Message{llms.UserText("Reply with exactly the word: pong")},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(strings.ToLower(resp.Text), "pong") {
		t.Fatalf("unexpected reply %q", resp.Text)
	}
	if resp.Usage.InputTokens == 0 || resp.Usage.OutputTokens == 0 {
		t.Fatalf("usage not populated: %+v", resp.Usage)
	}
	t.Logf("OK chat: %q usage=%+v", resp.Text, resp.Usage)
}

func TestIntegrationVertexAnthropicToolUse(t *testing.T) {
	c := newVertexClient(t)
	weather := llms.ToolDefinition{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	first, err := c.Complete(ctx, llms.ChatRequest{
		Purpose: llms.PurposeChat,
		// Auto tool choice, not forced: newer Claude models (seen: Opus 5.5)
		// reject tool_choice "tool"/"any". Forced choice is covered by the
		// first-party anthropic live test; this test is about the Vertex
		// transport/auth path.
		System:    []llms.ContentPart{llms.Text("Always use the get_weather tool for weather questions.")},
		Messages:  []llms.Message{llms.UserText("What's the weather in Paris?")},
		Tools:     []llms.ToolDefinition{weather},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("Complete (tool call): %v", err)
	}
	if len(first.ToolCalls) == 0 || first.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("expected a get_weather call, got %+v", first.Message)
	}
	tc := first.ToolCalls[0]
	final, err := c.Complete(ctx, llms.ChatRequest{
		Purpose: llms.PurposeChat,
		System:  []llms.ContentPart{llms.Text("Always use the get_weather tool for weather questions.")},
		Messages: []llms.Message{
			llms.UserText("What's the weather in Paris?"),
			first.Message,
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: tc.ID, Content: `{"city":"Paris","temp_c":18,"summary":"clear"}`}),
		},
		Tools:     []llms.ToolDefinition{weather},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("Complete (final): %v", err)
	}
	if final.Text == "" {
		t.Fatalf("no final answer after tool result: %+v", final.Message)
	}
	t.Logf("OK tool-use: %s(%s) -> %q", tc.Name, tc.Parameters, final.Text)
}

func TestIntegrationVertexAnthropicToolLoopConvergence(t *testing.T) {
	livetest.ToolLoopConvergence(t, newVertexClient(t))
}

func TestIntegrationVertexAnthropicCompactSummarize(t *testing.T) {
	livetest.CompactSummarize(t, newVertexClient(t), nil)
}

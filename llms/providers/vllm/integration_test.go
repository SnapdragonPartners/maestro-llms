//go:build integration

package vllm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/vllm"
)

// envBase returns the vLLM base URL from MAESTRO_VLLM, or "" to signal
// "skip this test" — matching the pattern other live tests use
// (OPENAI_API_KEY, OLLAMA_HOST, MAESTRO_ANTHROPIC_API_KEY).
func envBase() string { return os.Getenv("MAESTRO_VLLM") }

// envModel returns MAESTRO_VLLM_MODEL when set, otherwise empty so the
// caller knows to autodetect via ListModels.
func envModel() string { return os.Getenv("MAESTRO_VLLM_MODEL") }

// hclient is the shared httptest-tolerant client.
func hclient() *http.Client { return &http.Client{Timeout: 60 * time.Second} }

func newVLLMOrSkip(t *testing.T) (*vllm.Client, string) {
	t.Helper()
	base := envBase()
	if base == "" {
		t.Skip("MAESTRO_VLLM not set; skipping vLLM live test")
	}
	model := envModel()
	if model == "" {
		// Auto-detect: ask the server what it's serving and pick the first.
		probe, err := vllm.New(
			vllm.WithBaseURL(base),
			vllm.WithModel("placeholder"), // mandatory at construction; replaced after autodetect
			vllm.WithHTTPClient(hclient()),
		)
		if err != nil {
			t.Skipf("vLLM not constructable for autodetect: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		models, err := probe.ListModels(ctx)
		if err != nil {
			t.Skipf("vLLM not reachable at %s: %v", base, err)
		}
		if len(models) == 0 {
			t.Skipf("vLLM at %s returned no models", base)
		}
		model = models[0].ID
	}
	c, err := vllm.New(
		vllm.WithBaseURL(base),
		vllm.WithModel(model),
		vllm.WithHTTPClient(hclient()),
	)
	if err != nil {
		t.Fatalf("vllm.New: %v", err)
	}
	return c, model
}

func TestIntegrationVLLMChat(t *testing.T) {
	c, model := newVLLMOrSkip(t)
	t.Logf("vLLM live test against %s, model %s", envBase(), model)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	temp := float32(0.0)
	resp, err := c.Complete(ctx, llms.ChatRequest{
		Messages:    []llms.Message{llms.UserText("In one sentence, what is the capital of France?")},
		MaxTokens:   80,
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text == "" {
		t.Fatalf("empty response text; full resp: %+v", resp)
	}
	if !strings.Contains(strings.ToLower(resp.Text), "paris") {
		t.Errorf("expected 'Paris' in response, got %q", resp.Text)
	}
	if resp.Usage.InputTokens == 0 || resp.Usage.OutputTokens == 0 {
		t.Errorf("expected non-zero usage, got %+v", resp.Usage)
	}
}

func TestIntegrationVLLMListModels(t *testing.T) {
	c, _ := newVLLMOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := c.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("no models reported")
	}
	t.Logf("vLLM serving %d model(s):", len(models))
	for _, m := range models {
		t.Logf("  - %s (loaded %s)", m.ID, m.Created.Format(time.RFC3339))
		if m.Family != "" {
			t.Errorf("vLLM ModelInfo.Family must be empty per ADR-0015, got %q", m.Family)
		}
	}
}

func TestIntegrationVLLMToolUse(t *testing.T) {
	c, model := newVLLMOrSkip(t)
	t.Logf("vLLM tool-use test against %s, model %s", envBase(), model)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	temp := float32(0.0)
	weather := llms.ToolDefinition{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}
	first, err := c.Complete(ctx, llms.ChatRequest{
		Messages:    []llms.Message{llms.UserText("Use the get_weather tool to look up the weather in Paris. Respond only with the tool call.")},
		Tools:       []llms.ToolDefinition{weather},
		ToolChoice:  llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "get_weather"},
		MaxTokens:   200,
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	if len(first.ToolCalls) == 0 {
		t.Skipf("model %s did not emit a tool call (model-dependent): %q", model, first.Text)
	}
	tc := first.ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Fatalf("first ToolCall = %+v, want get_weather", tc)
	}

	// Feed back a synthetic tool result, ask the model to summarize.
	final, err := c.Complete(ctx, llms.ChatRequest{
		Messages: []llms.Message{
			llms.UserText("Use the get_weather tool to look up the weather in Paris."),
			first.Message,
			llms.ToolResultMessage(llms.ToolResult{
				ToolCallID: tc.ID,
				Content:    `{"city":"Paris","temp_c":18,"summary":"clear"}`,
			}),
		},
		Tools:       []llms.ToolDefinition{weather},
		MaxTokens:   200,
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if final.Text == "" {
		t.Fatalf("empty final text; full resp: %+v", final)
	}
	if !strings.Contains(strings.ToLower(final.Text), "paris") {
		t.Errorf("final text does not mention Paris: %q", final.Text)
	}
}

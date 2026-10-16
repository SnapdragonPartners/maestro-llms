//go:build integration

package vllm_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/vllm"
)

// LunaRoute is a hosted OpenAI-compatible router (issue #54). These tests
// skip unless LUNAROUTE_API_KEY is set. LunaRoute enforces a per-key
// concurrency limit, so nothing here calls t.Parallel; run with
// -test.parallel=1 if other packages share the key.
const (
	lunaRouteDefaultBaseURL = "https://gw.lunaroute.com/v1"
	// One GLM and one other tool-capable reasoning model. No Qwen model was
	// in the catalog when this was written; override with LUNAROUTE_MODELS.
	lunaRouteDefaultModels = "glm-5.3,deepseek-4.1-flash"
)

func lunaRouteBaseURL() string {
	if u := os.Getenv("LUNAROUTE_BASE_URL"); u != "" {
		return u
	}
	return lunaRouteDefaultBaseURL
}

func lunaRouteModels() []string {
	v := os.Getenv("LUNAROUTE_MODELS")
	if v == "" {
		v = lunaRouteDefaultModels
	}
	var out []string
	for _, m := range strings.Split(v, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func newLunaRoute(t *testing.T, key, model string) *vllm.Client {
	t.Helper()
	c, err := vllm.New(
		vllm.WithBaseURL(lunaRouteBaseURL()),
		vllm.WithAPIKey(key),
		vllm.WithModel(model),
		vllm.WithHTTPClient(hclient()),
	)
	if err != nil {
		t.Fatalf("vllm.New: %v", err)
	}
	return c
}

func lunaRouteKeyOrSkip(t *testing.T) string {
	t.Helper()
	key := os.Getenv("LUNAROUTE_API_KEY")
	if key == "" {
		t.Skip("LUNAROUTE_API_KEY not set; skipping LunaRoute live test")
	}
	return key
}

// TestIntegrationLunaRoute runs, per model: a forced-tool round trip, a
// ToolChoiceRequired call, an answer-only/reasoning-usage check, and the
// shared toolloop convergence scenario.
func TestIntegrationLunaRoute(t *testing.T) {
	key := lunaRouteKeyOrSkip(t)
	for _, model := range lunaRouteModels() {
		t.Run(model, func(t *testing.T) {
			c := newLunaRoute(t, key, model)
			t.Run("ToolRoundTrip", func(t *testing.T) { lunaRouteToolRoundTrip(t, c) })
			t.Run("ToolChoiceRequired", func(t *testing.T) { lunaRouteToolChoiceRequired(t, c) })
			t.Run("Reasoning", func(t *testing.T) { lunaRouteReasoning(t, c) })
			t.Run("ToolLoopConvergence", func(t *testing.T) { livetest.ToolLoopConvergence(t, c) })
		})
	}
}

var lunaRouteWeather = llms.ToolDefinition{
	Name:        "get_weather",
	Description: "Get the current weather for a city.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
}

func lunaRouteToolRoundTrip(t *testing.T, c *vllm.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	user := llms.UserText("Use the get_weather tool to look up the weather in Paris.")
	first, err := c.Complete(ctx, llms.ChatRequest{
		Messages:   []llms.Message{user},
		Tools:      []llms.ToolDefinition{lunaRouteWeather},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "get_weather"},
		MaxTokens:  1024,
	})
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	if len(first.ToolCalls) == 0 {
		t.Fatalf("forced tool choice produced no tool call; text=%q stop=%q", first.Text, first.StopReason)
	}
	tc := first.ToolCalls[0]
	var args struct{ City string }
	if tc.Name != "get_weather" || tc.ID == "" || json.Unmarshal(tc.Parameters, &args) != nil {
		t.Fatalf("tool call = %+v (params %s), want get_weather with an ID and JSON object args", tc, tc.Parameters)
	}
	if !strings.Contains(strings.ToLower(args.City), "paris") {
		t.Errorf("city = %q, want Paris", args.City)
	}

	final, err := c.Complete(ctx, llms.ChatRequest{
		Messages: []llms.Message{
			user,
			first.Message,
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: tc.ID, Content: `{"city":"Paris","temp_c":18,"summary":"clear"}`}),
		},
		Tools:     []llms.ToolDefinition{lunaRouteWeather},
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if !strings.Contains(final.Text, "18") {
		t.Errorf("final text does not use the tool result (18°C): %q", final.Text)
	}
}

func lunaRouteToolChoiceRequired(t *testing.T, c *vllm.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := c.Complete(ctx, llms.ChatRequest{
		Messages:   []llms.Message{llms.UserText("Say hello.")},
		Tools:      []llms.ToolDefinition{lunaRouteWeather},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceRequired},
		MaxTokens:  1024,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) == 0 {
		t.Fatalf("ToolChoiceRequired produced no tool call; text=%q stop=%q", resp.Text, resp.StopReason)
	}
}

// Reasoning models on LunaRoute return their reasoning in a separate
// reasoning_content field. resp.Text must be the answer only, and the
// reasoning must be counted in Usage (ADR-0016).
func lunaRouteReasoning(t *testing.T, c *vllm.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := c.Complete(ctx, llms.ChatRequest{
		Messages:  []llms.Message{llms.UserText("What is 17*23? Reply with just the number.")},
		MaxTokens: 2048,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := strings.TrimSpace(resp.Text); got != "391" {
		t.Errorf("Text = %q, want answer only (391)", resp.Text)
	}
	if strings.Contains(resp.Text, "<think>") {
		t.Errorf("Text contains inline reasoning: %q", resp.Text)
	}
	u := resp.Usage
	t.Logf("usage: %+v", u)
	if u.BillableOutputTokens != u.OutputTokens+u.ReasoningTokens {
		t.Errorf("BillableOutputTokens = %d, want OutputTokens+ReasoningTokens = %d", u.BillableOutputTokens, u.OutputTokens+u.ReasoningTokens)
	}
}

func TestIntegrationLunaRouteErrors(t *testing.T) {
	key := lunaRouteKeyOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}, MaxTokens: 8}
	models := lunaRouteModels()

	cases := []struct {
		name string
		c    *vllm.Client
		want llms.ErrorKind
	}{
		{"BadKey", newLunaRoute(t, "lr-invalid-key", models[0]), llms.ErrorKindAuth},
		{"UnknownModel", newLunaRoute(t, key, "no-such-model-54"), llms.ErrorKindBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.c.Complete(ctx, req)
			var pe *llms.ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *llms.ProviderError", err)
			}
			if pe.Kind != tc.want || llms.Retryable(err) {
				t.Fatalf("kind = %q retryable=%v (%v), want %q non-retryable", pe.Kind, llms.Retryable(err), err, tc.want)
			}
		})
	}
}

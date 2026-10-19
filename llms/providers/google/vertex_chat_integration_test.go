//go:build integration

package google_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials"
	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
)

// Live Gemini-chat-on-Vertex tests (#104, ADR-0028). Skipped unless
// VERTEX_PROJECT is set. Like the embeddings test, they load Application
// Default Credentials themselves and pass them in: the package does no ADC
// discovery (ADR-0009).
//
//	VERTEX_PROJECT               required
//	VERTEX_GEMINI_LOCATION       default "global"
//	VERTEX_GEMINI_MODEL          default "gemini-2.5-flash"
//	VERTEX_GEMINI_LEVEL_MODEL    default "gemini-3-flash-preview"
func newVertexGemini(t *testing.T, model string, extra ...google.Option) *google.Client {
	t.Helper()
	project := os.Getenv("VERTEX_PROJECT")
	if project == "" {
		t.Skip("VERTEX_PROJECT not set")
	}
	location := os.Getenv("VERTEX_GEMINI_LOCATION")
	if location == "" {
		location = "global"
	}
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err != nil {
		t.Skipf("no Google credentials (run `gcloud auth application-default login`): %v", err)
	}
	c, err := google.New(append([]google.Option{
		google.WithModel(model),
		google.WithVertex(project, location),
		google.WithCredentials(creds),
	}, extra...)...)
	if err != nil {
		t.Fatalf("New (Vertex): %v", err)
	}
	return c
}

func vertexModel() string {
	if m := os.Getenv("VERTEX_GEMINI_MODEL"); m != "" {
		return m
	}
	return "gemini-2.5-flash"
}

func TestIntegrationVertexGeminiChat(t *testing.T) {
	c := newVertexGemini(t, vertexModel())
	temp := float32(0)
	resp := complete(t, c, llms.ChatRequest{
		Purpose:     llms.PurposeChat,
		System:      []llms.ContentPart{llms.Text("Answer with exactly one lowercase word.")},
		Messages:    []llms.Message{llms.UserText("Reply with the single word: pong")},
		MaxTokens:   256,
		Temperature: &temp,
	})
	if resp.Message.Role != llms.RoleAssistant || resp.Text == "" {
		t.Fatalf("unexpected response: %+v", resp.Message)
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Fatalf("expected positive usage, got %+v", resp.Usage)
	}
	t.Logf("OK Vertex chat: text=%q stop=%q in=%d out=%d",
		resp.Text, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}

// ADR-0029: an injected client with no Google auth of its own (as a PSC
// client would be) is authenticated with WithCredentials.
func TestIntegrationVertexGeminiInjectedClient(t *testing.T) {
	var used atomic.Bool
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used.Store(true)
		return http.DefaultTransport.RoundTrip(r)
	})}
	c := newVertexGemini(t, vertexModel(), google.WithHTTPClient(hc))
	resp := complete(t, c, llms.ChatRequest{
		Messages:  []llms.Message{llms.UserText("Reply with the single word: pong")},
		MaxTokens: 256,
	})
	if !used.Load() {
		t.Fatal("the injected client was not used")
	}
	t.Logf("OK Vertex chat via an injected unauthenticated client: %q", resp.Text)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The tool round trip on Vertex, including the thought-signature replay
// (ADR-0010): gemini-2.5-flash thinks by default, so the function call
// carries a signature that must be sent back with the result.
func TestIntegrationVertexGeminiToolUse(t *testing.T) {
	c := newVertexGemini(t, vertexModel())
	weather := llms.ToolDefinition{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}
	system := []llms.ContentPart{llms.Text("Use the get_weather tool when asked about weather.")}
	first := complete(t, c, llms.ChatRequest{
		Purpose:    llms.PurposeChat,
		System:     system,
		Messages:   []llms.Message{llms.UserText("What's the weather in Paris? Use the tool.")},
		Tools:      []llms.ToolDefinition{weather},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "get_weather"},
		MaxTokens:  512,
	})
	if len(first.ToolCalls) == 0 {
		t.Fatalf("model did not call the tool: %+v", first.Message)
	}
	tc := first.ToolCalls[0]
	final := complete(t, c, llms.ChatRequest{
		Purpose: llms.PurposeChat,
		System:  system,
		Messages: []llms.Message{
			llms.UserText("What's the weather in Paris? Use the tool."),
			first.Message,
			llms.ToolResultMessage(llms.ToolResult{
				ToolCallID: tc.ID,
				Content:    `{"city":"Paris","temp_c":18,"summary":"clear"}`,
			}),
		},
		Tools:     []llms.ToolDefinition{weather},
		MaxTokens: 512,
	})
	if final.Text == "" {
		t.Fatalf("no final answer after tool result: %+v", final.Message)
	}
	t.Logf("OK Vertex tool-use: call=%s(%s) signature=%dB -> final=%q",
		tc.Name, tc.Parameters, len(tc.ProviderSignature), final.Text)
}

func TestIntegrationVertexGeminiToolLoopConvergence(t *testing.T) {
	livetest.ToolLoopConvergence(t, newVertexGemini(t, vertexModel()))
}

func TestIntegrationVertexGeminiLongToolConversation(t *testing.T) {
	livetest.LongToolConversation(t, newVertexGemini(t, vertexModel()))
}

func TestIntegrationVertexGeminiToolLoopNoTools(t *testing.T) {
	livetest.ToolLoopNoTools(t, newVertexGemini(t, vertexModel()))
}

// ADR-0022 on Vertex: BudgetTokens -> thinkingBudget (Gemini 2.5).
func TestIntegrationVertexGeminiReasoningBudget(t *testing.T) {
	livetest.ReasoningControl(t, newVertexGemini(t, vertexModel()),
		livetest.Same(llms.ReasoningConfig{BudgetTokens: 128}),
		livetest.Same(llms.ReasoningConfig{BudgetTokens: 4096}),
	)
}

// ADR-0022 on Vertex: Effort -> thinkingLevel (Gemini 3).
func TestIntegrationVertexGeminiReasoningEffort(t *testing.T) {
	m := os.Getenv("VERTEX_GEMINI_LEVEL_MODEL")
	if m == "" {
		m = "gemini-3-flash-preview"
	}
	livetest.ReasoningControl(t, newVertexGemini(t, m),
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortLow}),
		livetest.Same(llms.ReasoningConfig{Effort: llms.ReasoningEffortHigh}),
	)
}

// ADR-0023 on Vertex: implicit caching is best effort, so only the
// invariant (cache reads are a subset of InputTokens) is asserted.
func TestIntegrationVertexGeminiCachedInput(t *testing.T) {
	livetest.CachedInput(t, newVertexGemini(t, vertexModel()), false)
}

// ADR-0024 on Vertex: the nested override deep-merges into the Vertex body.
func TestIntegrationVertexGeminiProviderParams(t *testing.T) {
	livetest.ProviderParamsOverride(t, newVertexGemini(t, vertexModel()), map[string]any{
		"generationConfig": map[string]any{"maxOutputTokens": 16},
	})
}

// ADR-0026 on Vertex: the unparenthesized overflow wording classifies as
// ErrorKindContextLength. The model is fixed; the prompt is sized to its
// 1,048,576-token window.
func TestIntegrationVertexGeminiContextOverflow(t *testing.T) {
	livetest.ContextOverflow(t, newVertexGemini(t, "gemini-2.5-flash"), 1_100_000)
}

// ADR-0012 on Vertex: the publisher catalog lists with stripped IDs that
// round-trip into WithModel.
func TestIntegrationVertexGeminiListModels(t *testing.T) {
	c := newVertexGemini(t, vertexModel())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	models, err := c.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	// The raw name carries the Vertex publisher prefix; the ID must not.
	if raw, ok := models[0].Raw.(*genai.Model); ok {
		t.Logf("raw name %q -> ID %q", raw.Name, models[0].ID)
	}
	gemini := 0
	for i := range models {
		if strings.Contains(models[i].ID, "/") {
			t.Fatalf("ID %q still has a resource prefix", models[i].ID)
		}
		if models[i].Family != "" {
			gemini++
		}
		if i < 5 {
			t.Logf("model %q family=%q", models[i].ID, models[i].Family)
		}
	}
	if gemini == 0 {
		t.Fatalf("no Gemini-family models among %d listed", len(models))
	}
	newer, ok := google.LatestInFamily("gemini-1.5-flash", models)
	t.Logf("OK Vertex ListModels: %d models, %d Gemini; latest flash after 1.5: %q (%v)",
		len(models), gemini, newer.ID, ok)
}

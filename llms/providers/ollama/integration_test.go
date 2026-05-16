//go:build integration

package ollama_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
)

func host() string {
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		return h
	}
	return "http://localhost:11434"
}

func modelName() string {
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		return m
	}
	return "ministral-3:14b-instruct-2512-fp16"
}

// skipIfDown skips when no local Ollama server is reachable (Ollama needs no
// API key, so reachability is the gate).
func skipIfDown(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, host()+"/api/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Ollama not reachable at %s: %v", host(), err)
	}
	_ = resp.Body.Close()
}

func complete(t *testing.T, c llms.ChatClient, req llms.ChatRequest) llms.ChatResponse {
	t.Helper()
	var resp llms.ChatResponse
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		// Per-attempt deadline: a shared context across retries means a
		// first slow/stuck attempt poisons the rest with an already-expired
		// context instead of giving each try a fair, bounded window.
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			resp, err = c.Complete(ctx, req)
		}()
		if err == nil || !llms.Retryable(err) {
			break
		}
		t.Logf("attempt %d retryable: %v", attempt+1, err)
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return resp
}

func TestIntegrationOllamaChat(t *testing.T) {
	skipIfDown(t)
	c, err := ollama.New(ollama.WithBaseURL(host()), ollama.WithModel(modelName()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	temp := float32(0)
	resp := complete(t, c, llms.ChatRequest{
		Purpose:  llms.PurposeChat,
		System:   []llms.ContentPart{llms.Text("Answer with exactly one lowercase word.")},
		Messages: []llms.Message{llms.UserText("Reply with the single word: pong")},
		// CI uses a non-reasoning model (llama3.2:1b). 256 gives headroom
		// if OLLAMA_MODEL is overridden with a model that emits some
		// preamble; reasoning models still need think:false / non-reasoning.
		MaxTokens:   256,
		Temperature: &temp,
	})
	if resp.Message.Role != llms.RoleAssistant || resp.Text == "" {
		t.Fatalf("unexpected response: %+v", resp.Message)
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Fatalf("expected positive usage, got %+v", resp.Usage)
	}
	t.Logf("OK chat: text=%q stop=%q in=%d out=%d",
		resp.Text, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}

// TestIntegrationOllamaToolUse exercises the full live tool round trip
// against the local model: tool offered -> tool_call -> result fed back ->
// coherent final answer. Proves the ollama api translation end to end.
func TestIntegrationOllamaToolUse(t *testing.T) {
	skipIfDown(t)
	c, err := ollama.New(ollama.WithBaseURL(host()), ollama.WithModel(modelName()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	weather := llms.ToolDefinition{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}
	first := complete(t, c, llms.ChatRequest{
		Purpose:   llms.PurposeChat,
		System:    []llms.ContentPart{llms.Text("Use the get_weather tool when asked about weather.")},
		Messages:  []llms.Message{llms.UserText("What's the weather in Paris? Use the tool.")},
		Tools:     []llms.ToolDefinition{weather},
		MaxTokens: 512, // bound generation regardless of model verbosity
	})
	if len(first.ToolCalls) == 0 {
		t.Skipf("local model %s did not emit a tool call (model-dependent): %q", modelName(), first.Text)
	}
	tc := first.ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Fatalf("unexpected tool: %q", tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Parameters, &args); err != nil {
		t.Fatalf("tool args not valid JSON: %s (%v)", tc.Parameters, err)
	}

	final := complete(t, c, llms.ChatRequest{
		Purpose: llms.PurposeChat,
		System:  []llms.ContentPart{llms.Text("Use the get_weather tool when asked about weather.")},
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
	if !strings.Contains(strings.ToLower(final.Text), "paris") {
		t.Logf("note: final answer doesn't mention Paris (model wording): %q", final.Text)
	}
	t.Logf("OK tool-use: call=%s(%s) -> final=%q", tc.Name, tc.Parameters, final.Text)
}

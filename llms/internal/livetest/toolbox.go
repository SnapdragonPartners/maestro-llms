package livetest

import (
	"context"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

const toolboxSecret = "MARLIN-SPRUCE-77"

// ToolHistoryAfterRemoval checks that the provider accepts a transcript
// whose history holds a real call and result for a tool that is no longer
// in the request's Tools (ADR-0047): the state after a toolbox is put away,
// and after the loop honors a call to a declared but unadvertised tool.
//
// Phase 1 runs one tool turn (auto choice: forcing a tool conflicts
// with Anthropic thinking) with read_secret and get_time offered,
// so the history carries the provider's own call (including any provider
// signature). Phase 2 sends that transcript, which ends with the tool
// result, with only get_time offered, as the loop does on the turn after a
// put-away. The provider must accept it; whether the model then uses the
// removed tool's result is model behavior, logged rather than asserted.
func ToolHistoryAfterRemoval(t *testing.T, c llms.ChatClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	client := withRetry(c)

	readSecret := toolloop.Tool{
		Definition: toolDef("read_secret", "Read the secret word from the vault.", `{"type":"object","properties":{}}`),
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: "The secret word is " + toolboxSecret + "."}, nil
		},
	}
	getTime := toolloop.Tool{
		Definition: toolDef("get_time", "Return the current UTC time.", `{"type":"object","properties":{}}`),
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: "2026-11-14T12:00:00Z"}, nil
		},
	}
	base := llms.ChatRequest{
		System: []llms.ContentPart{llms.Text(
			"You answer questions using tools when needed. Once you know the answer, reply in plain text.")},
		Messages: []llms.Message{llms.UserText(
			"What is the secret word in the vault? Call read_secret to find out, then reply with just the word.")},
		MaxTokens: maxTokens,
	}

	var tr trace
	out := toolloop.Run(ctx, toolloop.Config{
		Client:        client,
		Request:       base,
		Tools:         []toolloop.Tool{readSecret, getTime},
		MaxIterations: 1,
		OnIteration:   tr.onIteration,
		OnToolCall:    tr.onToolCall,
	})
	tr.dump(t)
	if out.Kind != toolloop.OutcomeMaxIterations {
		t.Fatalf("phase 1: kind = %q, want max_iterations after one read_secret turn; err=%v", out.Kind, out.Err)
	}
	if !historyCalls(out.Messages, "read_secret") {
		t.Fatalf("phase 1: transcript has no read_secret call")
	}

	// Phase 2: read_secret is gone from Tools; its call and result remain.
	req := base
	req.Messages = out.Messages
	req.Tools = []llms.ToolDefinition{getTime.Definition}
	req.ToolChoice = llms.ToolChoice{Type: llms.ToolChoiceAuto}
	resp, err := client.Complete(ctx, req)
	if err != nil {
		t.Fatalf("phase 2: provider rejected history naming a tool absent from Tools: %v", err)
	}
	switch {
	case strings.Contains(strings.ToUpper(resp.Text), toolboxSecret):
		t.Logf("OK phase 2 answered %q", truncate(resp.Text, 80))
	case len(resp.ToolCalls) > 0:
		// Accepted; the model chose to call a tool instead of answering.
		t.Logf("OK phase 2 accepted; model called %q", resp.ToolCalls[0].Name)
	default:
		// Accepted, but the model did not use the result. Older Claude
		// models (Haiku 4.5, Opus 4.6) intermittently distrust the result of
		// a tool no longer offered; current ones use it (ADR-0047). The
		// provider accepting the history is what this scenario asserts.
		t.Logf("NOTE phase 2 accepted, but the answer ignores the removed tool's result: %q", truncate(resp.Text, 160))
	}
}

func historyCalls(msgs []llms.Message, name string) bool {
	for i := range msgs {
		for j := range msgs[i].Content {
			if p := &msgs[i].Content[j]; p.Type == llms.ContentToolCall && p.ToolCall != nil && p.ToolCall.Name == name {
				return true
			}
		}
	}
	return false
}

// ToolboxLoad runs the tool loop with toolboxes (ADR-0047). The secret is
// only reachable through read_secret, in the vault toolbox, which is not
// loaded; two other boxes are, and the cap leaves room for only two. The
// model must call get_toolbox("vault"), which puts away the least recently
// used box (clock, unless the model used get_time first), then call
// read_secret and answer.
func ToolboxLoad(t *testing.T, c llms.ChatClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()

	var readSecret bool
	tool := func(name, desc, result string) toolloop.Tool {
		return toolloop.Tool{
			Definition: toolDef(name, desc, `{"type":"object","properties":{}}`),
			Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
				if call.Name == "read_secret" {
					readSecret = true
				}
				return toolloop.ToolResult{Content: result}, nil
			},
		}
	}
	var tr trace
	out := toolloop.Run(ctx, toolloop.Config{
		Client: withRetry(c),
		Request: llms.ChatRequest{
			System: []llms.ContentPart{llms.Text(
				"You answer questions using tools. Some tools live in toolboxes you load with get_toolbox. " +
					"Once you know the answer, reply in plain text.")},
			Messages:  []llms.Message{llms.UserText("What is the secret word in the vault? Reply with just the word.")},
			MaxTokens: maxTokens,
		},
		Tools: []toolloop.Tool{
			tool("get_time", "Return the current UTC time.", "2026-11-14T12:00:00Z"),
			tool("read_note", "Read the shared note.", "Remember to water the plants."),
			tool("read_secret", "Read the secret word from the vault.", "The secret word is "+toolboxSecret+"."),
		},
		Toolboxes: &toolloop.Toolboxes{
			Boxes: []toolloop.Toolbox{
				{Name: "clock", Description: "Time and date.", Tools: []string{"get_time"}},
				{Name: "notes", Description: "Shared notes.", Tools: []string{"read_note"}},
				{Name: "vault", Description: "The secret vault.", Tools: []string{"read_secret"}},
			},
			Loaded:   []string{"clock", "notes"},
			MaxTools: 3, // get_toolbox + two boxes
		},
		MaxIterations: 6,
		OnIteration:   tr.onIteration,
		OnToolCall:    tr.onToolCall,
	})
	tr.dump(t)
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer; iterations=%d err=%v", out.Kind, out.Iterations, out.Err)
	}
	if !readSecret {
		t.Fatalf("read_secret was never executed")
	}
	if !strings.Contains(strings.ToUpper(out.Response.Text), toolboxSecret) {
		t.Errorf("answer %q does not contain the secret %q", out.Response.Text, toolboxSecret)
	}
	// Which of clock and notes is put away depends on whether the model
	// called get_time or read_note before loading the vault (each call marks
	// its box used), so assert only the invariant: vault plus exactly one of
	// the two, with vault the most recent.
	if got := out.LoadedToolboxes; len(got) != 2 || got[1] != "vault" || (got[0] != "clock" && got[0] != "notes") {
		t.Errorf("LoadedToolboxes = %v, want [clock vault] or [notes vault]", got)
	}
	t.Logf("OK toolbox run in %d tool iteration(s), usage in=%d out=%d",
		out.Iterations, out.TotalUsage.InputTokens, out.TotalUsage.OutputTokens)
}

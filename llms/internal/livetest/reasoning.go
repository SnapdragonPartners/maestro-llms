package livetest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// ReasoningReplay checks the ADR-0021 round-trip against a provider that
// returns reasoning and rejects a request whose replayed reasoning no longer
// matches the conversation (Anthropic preserved thinking with
// prefix_mismatch_behavior "error"; the caller builds c that way). It runs:
//
//  1. a multi-turn tool loop, which resends every earlier assistant turn with
//     its reasoning parts on each iteration, and must finish;
//  2. a follow-up on the unchanged transcript, which must be accepted;
//  3. the same follow-up with the first user turn edited, which must be
//     rejected, proving the provider is checking the replayed reasoning;
//  4. the edited transcript through llms.WithoutReasoning, which must be
//     accepted again.
func ReasoningReplay(t *testing.T, c llms.ChatClient) {
	t.Helper()
	if testing.Short() {
		t.Skip("live reasoning replay skipped under -short")
	}
	notes := map[string]string{
		"a": "Not here yet. Continue with note b.",
		"b": "End of chain. The answer word is " + codename + ".",
	}
	readNote := toolloop.Tool{
		Definition: toolDef("read_note", "Read a note by id.",
			`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			var a struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(call.Parameters, &a)
			if body, ok := notes[strings.TrimSpace(a.ID)]; ok {
				return toolloop.ToolResult{Content: body}, nil
			}
			return toolloop.ToolResult{Content: "no such note", IsError: true}, nil
		},
	}
	system := []llms.ContentPart{llms.Text(
		"Follow the chain of notes with read_note, one note at a time, until a note gives the answer word. " +
			"Before each tool call, think about what the notes so far tell you. " +
			"Then reply in plain text with the answer word.")}

	tools := []toolloop.Tool{readNote}
	msgs := reasoningLoop(t, c, system, tools)
	reasoningFollowUps(t, c, system, readNote.Definition, msgs)
}

// reasoningLoop is step 1: the tool loop, which must finish after replaying
// at least one assistant turn that carries reasoning. It returns the
// transcript.
func reasoningLoop(t *testing.T, c llms.ChatClient, system []llms.ContentPart, tools []toolloop.Tool) []llms.Message {
	t.Helper()
	// The scenario needs at least one thinking turn to replay, and the
	// model sometimes doesn't think (adaptive thinking) or a safety
	// classifier refuses a turn. Neither is an adapter fault, so retry a
	// few times before failing.
	const attempts = 3
	for attempt := 1; ; attempt++ {
		out, withReasoning := runReasoningLoop(t, c, system, tools)
		retryable := withReasoning == 0 ||
			(out.Kind == toolloop.OutcomeEmptyResponse && out.Response.StopReason == "refusal")
		if retryable && attempt < attempts {
			t.Logf("1. attempt %d: kind=%q stop=%q, %d assistant turn(s) with reasoning; retrying",
				attempt, out.Kind, out.Response.StopReason, withReasoning)
			continue
		}
		if out.Kind != toolloop.OutcomeFinalAnswer {
			t.Fatalf("1. tool loop: kind=%q stop=%q iterations=%d err=%v, want final_answer",
				out.Kind, out.Response.StopReason, out.Iterations, out.Err)
		}
		if out.Iterations < 2 {
			t.Fatalf("1. tool loop: iterations=%d, want >= 2 so reasoning was replayed at least once", out.Iterations)
		}
		if withReasoning == 0 {
			t.Fatalf("1. tool loop: no assistant turn carried a reasoning part in %d attempts; the provider returned none or the adapter dropped it", attempts)
		}
		t.Logf("1. tool loop OK: %d iteration(s), %d assistant turn(s) with reasoning", out.Iterations, withReasoning)
		return out.Messages
	}
}

// runReasoningLoop runs the tool loop once and counts the assistant turns
// that carry reasoning.
func runReasoningLoop(t *testing.T, c llms.ChatClient, system []llms.ContentPart, tools []toolloop.Tool) (toolloop.Outcome, int) {
	t.Helper()
	var tr trace
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	out := toolloop.Run(ctx, toolloop.Config{
		Client: withRetry(c),
		Request: llms.ChatRequest{
			System:    system,
			Messages:  []llms.Message{llms.UserText("Start at note a. What is the answer word?")},
			MaxTokens: 4096,
		},
		Tools:         tools,
		MaxIterations: 6,
		OnIteration:   tr.onIteration,
		OnToolCall:    tr.onToolCall,
	})
	tr.dump(t)
	return out, logReasoning(t, out.Messages)
}

// reasoningFollowUps is steps 2-4: unchanged transcript accepted, edited
// transcript rejected, edited transcript without reasoning accepted.
func reasoningFollowUps(t *testing.T, c llms.ChatClient, system []llms.ContentPart, tool llms.ToolDefinition, msgs []llms.Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	follow := func(msgs []llms.Message) error {
		_, err := withRetry(c).Complete(ctx, llms.ChatRequest{
			System:    system,
			Messages:  append(slices.Clone(msgs), llms.UserText("Repeat the answer word only.")),
			Tools:     []llms.ToolDefinition{tool},
			MaxTokens: 1024,
		})
		return err //nolint:wrapcheck // the test inspects the provider error as returned
	}
	if err := follow(msgs); err != nil {
		t.Fatalf("2. unchanged transcript rejected: %v", err)
	}
	t.Log("2. unchanged transcript accepted")

	edited := slices.Clone(msgs)
	edited[0] = llms.UserText("Start at note a, please. What is the answer word?")
	err := follow(edited)
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("3. edited transcript: err=%v, want a bad_request rejecting the stale reasoning's signature", err)
	}
	t.Logf("3. edited transcript rejected as expected: %v", err)

	if err := follow(llms.WithoutReasoning(edited)); err != nil {
		t.Fatalf("4. edited transcript without reasoning rejected: %v", err)
	}
	t.Log("4. edited transcript accepted after WithoutReasoning")
}

// logReasoning logs each reasoning part and returns how many assistant
// turns carry one.
func logReasoning(t *testing.T, msgs []llms.Message) int {
	t.Helper()
	n := 0
	for i := range msgs {
		m := &msgs[i]
		if m.Role == llms.RoleAssistant && hasReasoning(m) {
			n++
		}
		for j := range m.Content {
			if r := m.Content[j].Reasoning; m.Content[j].Type == llms.ContentReasoning && r != nil {
				t.Logf("message %d part %d: reasoning (%d bytes of provider data): %s", i, j, len(r.ProviderData), truncate(r.Text, 120))
			}
		}
	}
	return n
}

func hasReasoning(m *llms.Message) bool {
	for i := range m.Content {
		if m.Content[i].Type == llms.ContentReasoning {
			return true
		}
	}
	return false
}

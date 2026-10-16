// Package livetest holds provider-neutral live scenarios shared by the
// integration-tagged tests in each provider package. Each provider test
// builds its own client (credentials, model, base URL) and hands it here,
// so every provider runs the same scenario and checks the same assertions.
//
// Scenarios exercise llms/toolloop end to end against a real model with
// free (auto) tool choice. That is the path production consumers run, and
// the per-provider round-trip tests (which force the tool on turn one)
// don't cover it.
package livetest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

const (
	codename        = "HARBOR-FINCH-42"
	scenarioTimeout = 3 * time.Minute
	maxTokens       = 1024
)

// trace records what happened in a run so a failure is diagnosable from
// the test log alone.
type trace struct {
	lines []string
	empty []string // iterations whose response had neither text nor tool calls
}

func (tr *trace) onIteration(e toolloop.IterationEvent) {
	tr.lines = append(tr.lines, fmt.Sprintf("iter %d: %d tool call(s), stop=%q, text=%q",
		e.Index, e.NumToolCalls, e.Response.StopReason, truncate(e.Response.Text, 120)))
	if e.NumToolCalls == 0 && strings.TrimSpace(e.Response.Text) == "" {
		tr.empty = append(tr.empty, fmt.Sprintf("iter %d (stop=%q)", e.Index, e.Response.StopReason))
	}
}

func (tr *trace) onToolCall(e toolloop.ToolCallEvent) {
	tr.lines = append(tr.lines, fmt.Sprintf("  call %s(%s) -> isError=%v stop=%v %q",
		e.Call.Name, e.Call.Parameters, e.Result.IsError, e.Result.Stop, truncate(e.Result.Content, 80)))
}

func (tr *trace) dump(t *testing.T) {
	t.Helper()
	for _, l := range tr.lines {
		t.Log(l)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// withRetry wraps c so transient provider 5xx/429s don't make a
// multi-call scenario flaky. Only llms.Retryable errors are retried.
func withRetry(c llms.ChatClient) llms.ChatClient {
	return middleware.ChainChat(c, middleware.RetryChat(middleware.DefaultRetryConfig()))
}

func toolDef(name, desc, schema string) llms.ToolDefinition {
	return llms.ToolDefinition{Name: name, Description: desc, InputSchema: json.RawMessage(schema)}
}

func pathArg(call llms.ToolCall) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(call.Parameters, &a)
	return a.Path
}

// ToolLoopConvergence gives the model several general tools plus one
// terminal tool, with auto tool choice, and requires it to investigate
// and then converge on the terminal call within a bounded number of
// iterations (issue #51, dimension 1). The answer is only discoverable by
// reading a file, so a pass proves at least one real tool round trip
// before submission.
func ToolLoopConvergence(t *testing.T, c llms.ChatClient) {
	t.Helper()
	files := map[string]string{
		"README.md":          "# harbor\nInternal service. Deployment settings live under config/.",
		"config/deploy.yaml": "service: harbor\nregion: us-central1\ncodename: \"" + codename + "\"\nreplicas: 3\n",
		"src/main.go":        "package main\n\nfunc main() {}\n",
	}
	var readDeploy bool
	listFiles := toolloop.Tool{
		Definition: toolDef("list_files", "List every file path in the project.", `{"type":"object","properties":{}}`),
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: `["README.md","config/deploy.yaml","src/main.go"]`}, nil
		},
	}
	readFile := toolloop.Tool{
		Definition: toolDef("read_file", "Read a project file by path.",
			`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			p := pathArg(call)
			if p == "" {
				// Some models (seen: mistral-small3.2 on Ollama) send a
				// differently-named argument; say so, so they can self-correct.
				return toolloop.ToolResult{Content: fmt.Sprintf(`missing required argument "path"; got %s`, call.Parameters), IsError: true}, nil
			}
			body, ok := files[p]
			if !ok {
				return toolloop.ToolResult{Content: fmt.Sprintf("no such file %q; use list_files", p), IsError: true}, nil
			}
			if p == "config/deploy.yaml" {
				readDeploy = true
			}
			return toolloop.ToolResult{Content: body}, nil
		},
	}
	submit := toolloop.Tool{
		Definition: toolDef("submit_answer", "Submit the final answer. Call exactly once, when you are certain.",
			`{"type":"object","properties":{"codename":{"type":"string"}},"required":["codename"]}`),
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			var a struct {
				Codename string `json:"codename"`
			}
			// Rejections are model-visible and say what was wrong, so the
			// model can resubmit (the ADR-0017 validate-then-Stop pattern).
			_ = json.Unmarshal(call.Parameters, &a)
			if a.Codename == "" {
				return toolloop.ToolResult{Content: fmt.Sprintf(`missing required argument "codename"; got %s`, call.Parameters), IsError: true}, nil
			}
			if a.Codename != codename {
				return toolloop.ToolResult{Content: "incorrect codename; keep investigating with the tools", IsError: true}, nil
			}
			return toolloop.ToolResult{Content: "accepted", Stop: true}, nil
		},
	}

	var tr trace
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	out := toolloop.Run(ctx, toolloop.Config{
		Client: withRetry(c),
		Request: llms.ChatRequest{
			Purpose: llms.PurposeChat,
			System: []llms.ContentPart{llms.Text(
				"You investigate a software project using tools. You cannot see any file without calling a tool. " +
					"When you have found the answer, call submit_answer. Never answer in plain text.")},
			Messages:  []llms.Message{llms.UserText("What is this project's deployment codename?")},
			MaxTokens: maxTokens,
		},
		Tools:         []toolloop.Tool{listFiles, readFile, submit},
		MaxIterations: 8,
		OnIteration:   tr.onIteration,
		OnToolCall:    tr.onToolCall,
	})
	tr.dump(t)

	if out.Kind != toolloop.OutcomeTerminalTool {
		t.Fatalf("did not converge on submit_answer: kind=%q iterations=%d err=%v final text=%q",
			out.Kind, out.Iterations, out.Err, out.Response.Text)
	}
	if out.TerminalCall.Name != "submit_answer" {
		t.Fatalf("terminal call = %q, want submit_answer", out.TerminalCall.Name)
	}
	if !readDeploy || out.Iterations < 2 {
		t.Fatalf("submitted without investigating: readDeploy=%v iterations=%d", readDeploy, out.Iterations)
	}
	if len(tr.empty) > 0 {
		t.Errorf("empty response(s) mid-session: %v", tr.empty)
	}
	t.Logf("OK converged in %d tool iteration(s), usage in=%d out=%d",
		out.Iterations, out.TotalUsage.InputTokens, out.TotalUsage.OutputTokens)
}

// LongToolConversation drives a long sequential tool session: a chain of
// notes where each note names the next, so the hops cannot be
// parallelized. It finishes with a plain-text final answer (no terminal
// tool) and fails if any response in the session is empty, meaning no
// text and no tool calls (issue #51, dimension 2: the production
// empty-response incident).
//
// Empty responses are rare and not deterministically triggerable, so a
// pass is evidence, not proof. Skipped under -short.
func LongToolConversation(t *testing.T, c llms.ChatClient) {
	t.Helper()
	if testing.Short() {
		t.Skip("long live tool conversation skipped under -short")
	}
	// Opaque, unguessable IDs: each is learned only from the previous
	// note, and an ID not yet revealed is rejected, so a pass proves the
	// model followed the chain one hop per turn.
	chain := []string{"n1", "q7rk", "x2mb", "f9tw", "c4hz", "w8pd"}
	hops := len(chain)
	notes := make(map[string]string, hops)
	for i := 0; i < hops-1; i++ {
		notes[chain[i]] = fmt.Sprintf("Not here yet. Continue with note %s.", chain[i+1])
	}
	notes[chain[hops-1]] = "End of chain. The vault code is " + codename + "."
	revealed := map[string]bool{chain[0]: true}
	var visited []string

	readNote := toolloop.Tool{
		Definition: toolDef("read_note", "Read a note by id.",
			`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			var a struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(call.Parameters, &a)
			body, ok := notes[a.ID]
			if !ok || !revealed[a.ID] {
				return toolloop.ToolResult{Content: fmt.Sprintf("no note %q; only read ids you were given", a.ID), IsError: true}, nil
			}
			if !slices.Contains(visited, a.ID) { // first read only; re-reads are harmless
				visited = append(visited, a.ID)
			}
			if i := slices.Index(chain, a.ID); i+1 < hops {
				revealed[chain[i+1]] = true
			}
			return toolloop.ToolResult{Content: body}, nil
		},
	}

	var tr trace
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	out := toolloop.Run(ctx, toolloop.Config{
		Client: withRetry(c),
		Request: llms.ChatRequest{
			Purpose: llms.PurposeChat,
			System: []llms.ContentPart{llms.Text(
				"Follow the chain of notes with read_note, one note at a time, until a note gives the vault code. " +
					"Then reply in plain text with the vault code.")},
			Messages:  []llms.Message{llms.UserText("Start at note n1. What is the vault code?")},
			MaxTokens: maxTokens,
		},
		Tools:         []toolloop.Tool{readNote},
		MaxIterations: hops + 4,
		OnIteration:   tr.onIteration,
		OnToolCall:    tr.onToolCall,
	})
	tr.dump(t)

	if len(tr.empty) > 0 {
		t.Fatalf("empty response(s) in long tool session: %v (outcome kind=%q)", tr.empty, out.Kind)
	}
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind=%q iterations=%d err=%v, want final_answer", out.Kind, out.Iterations, out.Err)
	}
	if !slices.Equal(visited, chain) {
		t.Fatalf("notes visited = %v, want the full chain in order %v", visited, chain)
	}
	if out.Iterations < hops {
		t.Fatalf("iterations=%d, want >= %d (one per hop)", out.Iterations, hops)
	}
	if !strings.Contains(out.Response.Text, codename) {
		t.Fatalf("final answer %q does not contain the vault code", out.Response.Text)
	}
	t.Logf("OK %d-hop session, %d tool iteration(s), usage in=%d out=%d",
		hops, out.Iterations, out.TotalUsage.InputTokens, out.TotalUsage.OutputTokens)
}

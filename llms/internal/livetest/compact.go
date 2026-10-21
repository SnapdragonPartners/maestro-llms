package livetest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop/compact"
)

// CompactOnOverflow is the ADR-0030 live overflow scenario. The model calls
// read_log, whose first result is about words tokens (sized, as for
// ContextOverflow, above the model's window), so the next request overflows.
// compact.KeepLast with Turns 0 drops that turn, the loop retries with just
// the task, the model calls read_log again and gets a short log, and the run
// ends with a final answer naming the codename in it. The oversized request
// is rejected before inference, so it is not billed.
func CompactOnOverflow(t *testing.T, c llms.ChatClient, words int) {
	t.Helper()
	if testing.Short() {
		t.Skip("live compaction skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	reads := 0
	readLog := toolloop.Tool{
		Definition: toolDef("read_log", "Read the service log.", `{"type":"object","properties":{}}`),
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			reads++
			if reads == 1 {
				return toolloop.ToolResult{Content: strings.Repeat("hello ", words)}, nil
			}
			return toolloop.ToolResult{Content: "deploy finished; release codename " + codename}, nil
		},
	}
	keep, err := compact.KeepLast(compact.KeepLastConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tr := &trace{}
	out := toolloop.Run(ctx, toolloop.Config{
		Client: withRetry(c),
		Request: llms.ChatRequest{
			System:    []llms.ContentPart{llms.Text("Use the read_log tool to answer. Reply in one short sentence.")},
			Messages:  []llms.Message{llms.UserText("What release codename does the service log report?")},
			MaxTokens: maxTokens,
		},
		Tools:       []toolloop.Tool{readLog},
		Compact:     keep,
		OnIteration: tr.onIteration,
		OnToolCall:  tr.onToolCall,
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		tr.dump(t)
		t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
	}
	if out.Compactions != 1 || reads != 2 {
		tr.dump(t)
		t.Fatalf("Compactions = %d, read_log calls = %d; want 1 and 2", out.Compactions, reads)
	}
	if !strings.Contains(out.Response.Text, codename) {
		t.Errorf("final answer %q lacks %s", out.Response.Text, codename)
	}
}

// compactChain is CompactSummarize's lookup table: each result names the
// next key, so the model cannot batch the calls and the summaries must
// carry both the values and where the chain stands.
func compactChain() map[string]string {
	return map[string]string{
		"alpha": "7341. The next key to look up is beta.",
		"beta":  "2958. The next key to look up is gamma.",
		"gamma": "6612. That was the last key.",
	}
}

// CompactSummarize is the ADR-0030 live summarization scenario. The model
// follows a three-key lookup chain one call at a time; compact.Summarize
// (keeping the last turn, with a threshold of one token so it acts before
// every turn once there is a turn to drop) replaces older turns with a
// summary written by summarizer (c when nil). The final answer must still
// report every value, so the summaries carried them, and every compacted
// transcript must have been accepted by the provider. Each summary is
// logged.
func CompactSummarize(t *testing.T, c, summarizer llms.ChatClient) {
	t.Helper()
	if testing.Short() {
		t.Skip("live compaction skipped under -short")
	}
	if summarizer == nil {
		summarizer = c
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	chain := compactChain()
	lookup := toolloop.Tool{
		Definition: toolDef("lookup", "Look up the value stored under a key.",
			`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}`),
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			var args struct {
				Key string `json:"key"`
			}
			// Malformed arguments leave Key empty: "no such key" below.
			_ = json.Unmarshal(call.Parameters, &args)
			v, ok := chain[strings.ToLower(strings.TrimSpace(args.Key))]
			if !ok {
				return toolloop.ToolResult{Content: "no such key", IsError: true}, nil
			}
			return toolloop.ToolResult{Content: v}, nil
		},
	}
	sum, err := compact.Summarize(compact.SummarizeConfig{Client: withRetry(summarizer), KeepTurns: 1, AtTokens: 1, MaxTokens: maxTokens})
	if err != nil {
		t.Fatal(err)
	}
	logged := func(ctx context.Context, in toolloop.CompactInput) (toolloop.CompactResult, error) {
		res, err := sum(ctx, in)
		for i := range res.Messages {
			if m := &res.Messages[i]; m.Role == llms.RoleUser && len(m.Content) == 1 && strings.HasPrefix(m.Content[0].Text, "Summary of") {
				t.Logf("summary (%d -> %d messages): %q", len(in.Messages), len(res.Messages), truncate(m.Content[0].Text, 600))
			}
		}
		return res, err
	}
	tr := &trace{}
	out := toolloop.Run(ctx, toolloop.Config{
		Client: withRetry(c),
		Request: llms.ChatRequest{
			System: []llms.ContentPart{llms.Text("Use the lookup tool, one call per reply.")},
			Messages: []llms.Message{llms.UserText("Start by looking up the key alpha. Each result names the next key " +
				"to look up; follow the chain until the last key, then reply with every key and its value as " +
				"key=value, comma-separated.")},
			MaxTokens: maxTokens,
		},
		Tools:         []toolloop.Tool{lookup},
		Compact:       logged,
		MaxIterations: 8,
		OnIteration:   tr.onIteration,
		OnToolCall:    tr.onToolCall,
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		tr.dump(t)
		t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
	}
	t.Logf("compactions=%d iterations=%d usage=%+v compaction usage=%+v", out.Compactions, out.Iterations, out.TotalUsage, out.CompactionUsage)
	if out.Compactions < 1 || out.CompactionUsage.InputTokens == 0 {
		tr.dump(t)
		t.Fatalf("Compactions = %d, CompactionUsage = %+v; want at least one summarization with usage", out.Compactions, out.CompactionUsage)
	}
	for k, v := range map[string]string{"alpha": "7341", "beta": "2958", "gamma": "6612"} {
		if !strings.Contains(out.Response.Text, v) {
			tr.dump(t)
			t.Errorf("final answer %q lacks %s=%s", out.Response.Text, k, v)
		}
	}
}

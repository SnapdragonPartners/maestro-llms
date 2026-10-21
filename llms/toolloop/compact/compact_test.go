package compact_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop/compact"
)

// turn returns an assistant tool-call message with reasoning, and its
// tool-result message.
func turn(id, result string) []llms.Message {
	call := testllm.ToolCallResponse(id, "read", `{"path":"`+id+`"}`).Message
	call.Content = append([]llms.ContentPart{{
		Type:      llms.ContentReasoning,
		Reasoning: &llms.Reasoning{Provider: llms.ProviderAnthropic, Text: "thinking about " + id},
	}}, call.Content...)
	return []llms.Message{call, llms.ToolResultMessage(llms.ToolResult{ToolCallID: id, Content: result})}
}

// transcript is: task, t1, t2, nudge (user), t3.
func transcript() []llms.Message {
	msgs := []llms.Message{llms.UserText("task")}
	msgs = append(msgs, turn("t1", "one")...)
	msgs = append(msgs, turn("t2", "two")...)
	msgs = append(msgs, llms.UserText("nudge"))
	msgs = append(msgs, turn("t3", "three")...)
	return msgs
}

// ids lists each message as its role, or the tool-call ID it carries.
func ids(msgs []llms.Message) string {
	out := make([]string, len(msgs))
	for i := range msgs {
		out[i] = string(msgs[i].Role)
		for _, p := range msgs[i].Content {
			switch {
			case p.Type == llms.ContentText && msgs[i].Role == llms.RoleUser:
				out[i] = p.Text
			case p.ToolCall != nil:
				out[i] = "call:" + p.ToolCall.ID
			case p.ToolResult != nil:
				out[i] = "result:" + p.ToolResult.ToolCallID
			}
		}
	}
	return strings.Join(out, " ")
}

func overflow(msgs []llms.Message) toolloop.CompactInput {
	return toolloop.CompactInput{Reason: toolloop.CompactOverflow, Messages: msgs}
}

func mustKeepLast(t *testing.T, cfg compact.KeepLastConfig) toolloop.Compactor {
	t.Helper()
	c, err := compact.KeepLast(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestKeepLast_CutsAtTurnBoundaries(t *testing.T) {
	cases := []struct {
		name string
		want string // "" means no change
		msgs []llms.Message
		keep int
	}{
		// The nudge is t3's prompt, so it stays with t3.
		{"keep 1", "task nudge call:t3 result:t3", transcript(), 1},
		{"keep 2", "task call:t2 result:t2 nudge call:t3 result:t3", transcript(), 2},
		{"keep all", "", transcript(), 3},
		{"keep more than exist", "", transcript(), 9},
		{"keep 0", "task", transcript(), 0},
		// An unanswered prompt after the last turn is always kept.
		{"keep 0, pending prompt", "task again", append(transcript(), llms.UserText("again")), 0},
		{"keep 1, pending prompt", "task nudge call:t3 result:t3 again", append(transcript(), llms.UserText("again")), 1},
		{"head only", "", []llms.Message{llms.UserText("task")}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := ids(tc.msgs)
			res, err := mustKeepLast(t, compact.KeepLastConfig{Turns: tc.keep})(context.Background(), overflow(tc.msgs))
			if err != nil {
				t.Fatal(err)
			}
			if ids(tc.msgs) != before {
				t.Fatalf("input modified: %s -> %s", before, ids(tc.msgs))
			}
			if tc.want == "" {
				if res.Messages != nil {
					t.Fatalf("got %s, want no change", ids(res.Messages))
				}
				return
			}
			if got := ids(res.Messages); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if err := middleware.ValidateMessages(res.Messages); err != nil {
				t.Fatalf("result invalid: %v", err)
			}
			if res.Usage != (llms.Usage{}) {
				t.Errorf("Usage = %+v, want zero (no provider call)", res.Usage)
			}
		})
	}
}

func TestKeepLast_StripsReasoning(t *testing.T) {
	res, err := mustKeepLast(t, compact.KeepLastConfig{Turns: 1})(context.Background(), overflow(transcript()))
	if err != nil {
		t.Fatal(err)
	}
	for i := range res.Messages {
		for _, p := range res.Messages[i].Content {
			if p.Type == llms.ContentReasoning {
				t.Fatalf("message %d still has reasoning", i)
			}
		}
	}
}

func TestKeepLast_Threshold(t *testing.T) {
	msgs := transcript()
	before := func(in, out int) toolloop.CompactInput {
		return toolloop.CompactInput{Reason: toolloop.CompactBeforeTurn, Messages: msgs, LastUsage: llms.Usage{InputTokens: in, OutputTokens: out}}
	}
	cases := []struct {
		name string
		in   toolloop.CompactInput
		at   int
		want bool
	}{
		{"overflow-only ignores before-turn", before(1_000_000, 0), 0, false},
		{"below threshold", before(90, 9), 100, false},
		{"at threshold counts output", before(90, 10), 100, true},
		{"overflow always", overflow(msgs), 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := mustKeepLast(t, compact.KeepLastConfig{Turns: 1, AtTokens: tc.at})(context.Background(), tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Messages != nil; got != tc.want {
				t.Fatalf("compacted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConstructors_RejectBadConfig(t *testing.T) {
	if _, err := compact.KeepLast(compact.KeepLastConfig{Turns: -1}); err == nil {
		t.Error("KeepLast accepted negative Turns")
	}
	if _, err := compact.KeepLast(compact.KeepLastConfig{AtTokens: -1}); err == nil {
		t.Error("KeepLast accepted negative AtTokens")
	}
	if _, err := compact.Summarize(compact.SummarizeConfig{}); err == nil {
		t.Error("Summarize accepted a nil Client")
	}
	fake := &testllm.FakeChatClient{}
	for _, cfg := range []compact.SummarizeConfig{
		{Client: fake, KeepTurns: -1}, {Client: fake, AtTokens: -1}, {Client: fake, MaxTokens: -1},
	} {
		if _, err := compact.Summarize(cfg); err == nil {
			t.Errorf("Summarize accepted %+v", cfg)
		}
	}
}

func mustSummarize(t *testing.T, cfg compact.SummarizeConfig) toolloop.Compactor {
	t.Helper()
	c, err := compact.Summarize(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSummarize_ReplacesDroppedTurns(t *testing.T) {
	sum := testllm.TextResponse("  read t1 and t2  ")
	sum.Usage = llms.Usage{InputTokens: 50, OutputTokens: 5}
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{sum}}
	c := mustSummarize(t, compact.SummarizeConfig{Client: fake, KeepTurns: 1, MaxTokens: 300, Purpose: "compaction"})
	res, err := c(context.Background(), overflow(transcript()))
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res.Messages)
	if !strings.HasPrefix(got, "task Summary of the earlier part") || !strings.HasSuffix(got, "read t1 and t2 nudge call:t3 result:t3") {
		t.Fatalf("got %s", got)
	}
	if err := middleware.ValidateMessages(res.Messages); err != nil {
		t.Fatalf("result invalid: %v", err)
	}
	if res.Usage != sum.Usage {
		t.Errorf("Usage = %+v, want the summarizer's %+v", res.Usage, sum.Usage)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("summarizer calls = %d, want 1", len(calls))
	}
	req := calls[0]
	if req.MaxTokens != 300 || req.Purpose != "compaction" || len(req.Tools) != 0 {
		t.Errorf("request MaxTokens = %d, Purpose = %q, Tools = %d", req.MaxTokens, req.Purpose, len(req.Tools))
	}
	if len(req.System) != 1 || req.System[0].Text != compact.DefaultSummaryPrompt {
		t.Errorf("System = %+v, want the default prompt", req.System)
	}
	// The dropped turns (t1, t2) are rendered as text, without reasoning;
	// the head and the kept turn (t3 with its nudge) are not.
	if len(req.Messages) != 1 || req.Messages[0].Role != llms.RoleUser {
		t.Fatalf("Messages = %+v, want one user message", req.Messages)
	}
	text := req.Messages[0].Content[0].Text
	for _, want := range []string{`[assistant called tool read]` + "\n" + `{"path":"t1"}`, "[tool result from read]\none", "[tool result from read]\ntwo"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered history lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"thinking about", "t3", "task", "nudge"} {
		if strings.Contains(text, not) {
			t.Errorf("rendered history contains %q:\n%s", not, text)
		}
	}
}

func TestSummarize_Truncates(t *testing.T) {
	big := strings.Repeat("é", 50)
	msgs := []llms.Message{llms.UserText("task")}
	msgs = append(msgs, turn("t1", big)...)
	msgs = append(msgs, turn("t2", "small")...)
	cases := []struct {
		name      string
		limit     int
		truncated bool
	}{{"limit", 10, true}, {"default fits", 0, false}, {"no limit", -1, false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("s")}}
			c := mustSummarize(t, compact.SummarizeConfig{Client: fake, KeepTurns: 1, TruncateRunes: tc.limit, Prompt: "custom"})
			if _, err := c(context.Background(), overflow(msgs)); err != nil {
				t.Fatal(err)
			}
			req := fake.Calls()[0]
			text := req.Messages[0].Content[0].Text
			if req.System[0].Text != "custom" {
				t.Errorf("System = %q, want the custom prompt", req.System[0].Text)
			}
			if got := strings.Contains(text, strings.Repeat("é", 10)+" …[truncated 80 bytes]"); got != tc.truncated {
				t.Fatalf("truncated = %v, want %v:\n%s", got, tc.truncated, text)
			}
			if !tc.truncated && !strings.Contains(text, big) {
				t.Fatalf("full text missing:\n%s", text)
			}
		})
	}
}

func TestSummarize_NothingToDrop_NoCall(t *testing.T) {
	fake := &testllm.FakeChatClient{}
	res, err := mustSummarize(t, compact.SummarizeConfig{Client: fake, KeepTurns: 3})(context.Background(), overflow(transcript()))
	if err != nil || res.Messages != nil || len(fake.Calls()) != 0 {
		t.Fatalf("res = %+v, err = %v, calls = %d; want no change and no call", res, err, len(fake.Calls()))
	}
}

func TestSummarize_Errors(t *testing.T) {
	boom := errors.New("down")
	t.Run("client error", func(t *testing.T) {
		c := mustSummarize(t, compact.SummarizeConfig{Client: &testllm.FakeChatClient{Err: boom}, KeepTurns: 1})
		if _, err := c(context.Background(), overflow(transcript())); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the client error", err)
		}
	})
	t.Run("empty summary keeps usage", func(t *testing.T) {
		empty := testllm.TextResponse("  ")
		empty.Usage = llms.Usage{InputTokens: 40}
		empty.StopReason = "max_tokens"
		c := mustSummarize(t, compact.SummarizeConfig{Client: &testllm.FakeChatClient{Responses: []llms.ChatResponse{empty}}, KeepTurns: 1})
		res, err := c(context.Background(), overflow(transcript()))
		if err == nil || !strings.Contains(err.Error(), "max_tokens") || res.Usage.InputTokens != 40 || res.Messages != nil {
			t.Fatalf("res = %+v, err = %v; want an error naming the stop reason, with usage", res, err)
		}
	})
}

// TestRun_WithKeepLast drives a loop that overflows once with KeepLast as
// the hook: the compacted transcript is retried and the run finishes.
func TestRun_WithKeepLast(t *testing.T) {
	calls := 0
	client := &testllm.FakeChatClient{Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
		calls++
		switch {
		case calls <= 2:
			id := []string{"a", "b"}[calls-1]
			return testllm.ToolCallResponse(id, "read", `{}`), nil
		case len(req.Messages) > 3:
			return llms.ChatResponse{}, &llms.ProviderError{Kind: llms.ErrorKindContextLength, StatusCode: 400}
		default:
			return testllm.TextResponse("done"), nil
		}
	}}
	read := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "read"},
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: "data"}, nil
		},
	}
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: llms.ChatRequest{Messages: []llms.Message{llms.UserText("task")}},
		Tools:   []toolloop.Tool{read},
		Compact: mustKeepLast(t, compact.KeepLastConfig{Turns: 1}),
	})
	if out.Kind != toolloop.OutcomeFinalAnswer || out.Compactions != 1 {
		t.Fatalf("kind = %q, Compactions = %d; want final_answer, 1 (err=%v)", out.Kind, out.Compactions, out.Err)
	}
	if got := ids(out.Messages); got != "task call:b result:b assistant" {
		t.Fatalf("Messages = %s", got)
	}
}

// TestSummarize_RollsPreviousSummary: on the next compaction the previous
// summary is among the dropped messages; it is sent in full as the existing
// summary to update, apart from the new turns.
func TestSummarize_RollsPreviousSummary(t *testing.T) {
	first := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("alpha=" + strings.Repeat("7", 30))}}
	c := mustSummarize(t, compact.SummarizeConfig{Client: first, KeepTurns: 1, TruncateRunes: 10})
	res, err := c(context.Background(), overflow(transcript()))
	if err != nil {
		t.Fatal(err)
	}
	msgs := append(res.Messages, turn("t4", "four")...)
	second := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("rolled")}}
	c = mustSummarize(t, compact.SummarizeConfig{Client: second, KeepTurns: 1, TruncateRunes: 10})
	if _, err := c(context.Background(), overflow(msgs)); err != nil {
		t.Fatal(err)
	}
	text := second.Calls()[0].Messages[0].Content[0].Text
	if !strings.HasPrefix(text, "Existing summary. ") || !strings.Contains(text, ":\n\nalpha="+strings.Repeat("7", 30)+"\n\nNew turns to add") {
		t.Fatalf("previous summary not set apart in full:\n%s", text)
	}
	if !strings.Contains(text, "[tool result from read]\nthree") {
		t.Errorf("new turns missing:\n%s", text)
	}
	if strings.Contains(text, "Summary of the earlier part") {
		t.Errorf("header rendered:\n%s", text)
	}
}

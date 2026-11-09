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
		// The nudge is the in-flight prompt (t3 is still answering it), so
		// it is kept even when t3 is not (ADR-0038).
		{"keep 0", "task nudge", transcript(), 0},
		// A transcript ending with an assistant message has no turn in
		// flight, so no prompt is pinned.
		{"keep 0, answered", "task", append(transcript(), testllm.TextResponse("done").Message), 0},
		// An unanswered prompt after the last turn is always kept.
		{"keep 0, pending prompt", "task again", append(transcript(), llms.UserText("again")), 0},
		{"keep 1, pending prompt", "task nudge call:t3 result:t3 again", append(transcript(), llms.UserText("again")), 1},
		{"head only", "", []llms.Message{llms.UserText("task")}, 0},
		{"no user message", "", nil, 0},
		// Messages before the first user message are part of the head.
		{"leading assistant", "assistant task again", append([]llms.Message{testllm.TextResponse("hi").Message}, append(transcript(), llms.UserText("again"))...), 0},
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

// chat is a chat app's transcript: three answered questions, the new
// question, and three tool rounds the loop appended answering it.
func chat() []llms.Message {
	var msgs []llms.Message
	for _, q := range []string{"q1", "q2", "q3"} {
		msgs = append(msgs, llms.UserText(q), testllm.TextResponse("a-"+q).Message)
	}
	msgs = append(msgs, llms.UserText("NOW"))
	for _, id := range []string{"c1", "c2", "c3"} {
		msgs = append(msgs, turn(id, "res-"+id)...)
	}
	return msgs
}

// TestKeepLast_PinsInFlightPrompt: with chat history before it, the prompt
// the in-flight turns answer is kept, and the history is dropped first
// (#123, ADR-0038).
func TestKeepLast_PinsInFlightPrompt(t *testing.T) {
	cases := []struct {
		name string
		want string
		msgs []llms.Message
		keep int
	}{
		{"keep 2", "q1 NOW call:c2 result:c2 call:c3 result:c3", chat(), 2},
		{"keep 0", "q1 NOW", chat(), 0},
		// The prompt's own turn is kept, so nothing extra is pinned.
		{"keep 3", "q1 NOW call:c1 result:c1 call:c2 result:c2 call:c3 result:c3", chat(), 3},
		{"keep 4", "q1 q3 assistant NOW call:c1 result:c1 call:c2 result:c2 call:c3 result:c3", chat(), 4},
		// Consecutive user messages form one prompt.
		{"two-message prompt", "q1 NOW more call:c2 result:c2 call:c3 result:c3", withMore(chat()), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := mustKeepLast(t, compact.KeepLastConfig{Turns: tc.keep})(context.Background(), overflow(tc.msgs))
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(res.Messages); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if err := middleware.ValidateMessages(res.Messages); err != nil {
				t.Fatalf("result invalid: %v", err)
			}
		})
	}
}

// withMore inserts a second user message after NOW.
func withMore(msgs []llms.Message) []llms.Message {
	for i := range msgs {
		if ids(msgs[i:i+1]) == "NOW" {
			return append(msgs[:i+1:i+1], append([]llms.Message{llms.UserText("more")}, msgs[i+1:]...)...)
		}
	}
	panic("no NOW")
}

// TestSummarize_PinsInFlightPrompt: the in-flight prompt is neither
// summarized nor dropped. The summary goes right after the (non-summary)
// head, before the prompt, so a later compaction finds it among the dropped messages and
// rolls it, and it is never mistaken for the prompt.
func TestSummarize_PinsInFlightPrompt(t *testing.T) {
	first := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("S1")}}
	res, err := mustSummarize(t, compact.SummarizeConfig{Client: first, KeepTurns: 1})(context.Background(), overflow(chat()))
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res.Messages)
	if !strings.HasPrefix(got, "q1 Summary of the earlier part") || !strings.HasSuffix(got, "S1 NOW call:c3 result:c3") {
		t.Fatalf("got %s", got)
	}
	if verr := middleware.ValidateMessages(res.Messages); verr != nil {
		t.Fatalf("result invalid: %v", verr)
	}
	text := first.Calls()[0].Messages[0].Content[0].Text
	for _, want := range []string{"a-q1", "q2", "a-q3", "res-c1", "res-c2"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered history lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"NOW", "res-c3", "[user]\nq1"} {
		if strings.Contains(text, not) {
			t.Errorf("rendered history contains %q:\n%s", not, text)
		}
	}

	// Two more rounds, then compact again: S1 rolls, NOW stays pinned.
	msgs := append(res.Messages, turn("c4", "res-c4")...)
	msgs = append(msgs, turn("c5", "res-c5")...)
	second := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("S2")}}
	res, err = mustSummarize(t, compact.SummarizeConfig{Client: second, KeepTurns: 1})(context.Background(), overflow(msgs))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Messages); !strings.HasSuffix(got, "S2 NOW call:c5 result:c5") || strings.Contains(got, "S1") {
		t.Fatalf("got %s", got)
	}
	text = second.Calls()[0].Messages[0].Content[0].Text
	if !strings.HasPrefix(text, "Existing summary. ") || !strings.Contains(text, "S1") || strings.Contains(text, "NOW") {
		t.Fatalf("second summary request:\n%s", text)
	}
}

// TestRun_WithKeepLast_ChatHistory is #123's reproduction: a chat
// transcript, three tool rounds, then an overflow. The retried request
// still carries the new question.
func TestRun_WithKeepLast_ChatHistory(t *testing.T) {
	var history []llms.Message
	for _, q := range []string{"q1", "q2", "q3"} {
		history = append(history, llms.UserText(q), testllm.TextResponse("a-"+q).Message)
	}
	calls := 0
	var retried llms.ChatRequest
	client := &testllm.FakeChatClient{Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
		calls++
		switch {
		case calls <= 3:
			return testllm.ToolCallResponse([]string{"a", "b", "c"}[calls-1], "read", `{}`), nil
		case calls == 4:
			return llms.ChatResponse{}, &llms.ProviderError{Kind: llms.ErrorKindContextLength, StatusCode: 400}
		default:
			retried = req
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
		Request: llms.ChatRequest{Messages: append(history, llms.UserText("NOW"))},
		Tools:   []toolloop.Tool{read},
		Compact: mustKeepLast(t, compact.KeepLastConfig{Turns: 2}),
	})
	if out.Kind != toolloop.OutcomeFinalAnswer || out.Compactions != 1 {
		t.Fatalf("kind = %q, Compactions = %d; want final_answer, 1 (err=%v)", out.Kind, out.Compactions, out.Err)
	}
	if got := ids(retried.Messages); got != "q1 NOW call:b result:b call:c result:c" {
		t.Fatalf("retried request = %s", got)
	}
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

func TestSummaryText(t *testing.T) {
	if got, ok := compact.SummaryText(compact.SummaryMessage("facts")); !ok || got != "facts" {
		t.Fatalf("round trip = %q, %v", got, ok)
	}
	summary := compact.SummaryMessage("facts")
	for name, msg := range map[string]llms.Message{
		"plain user text": llms.UserText("facts"),
		"assistant":       {Role: llms.RoleAssistant, Content: summary.Content},
		"extra part":      {Role: llms.RoleUser, Content: append([]llms.ContentPart{llms.Text("x")}, summary.Content...)},
		"no content":      {Role: llms.RoleUser},
	} {
		if got, ok := compact.SummaryText(msg); ok {
			t.Errorf("%s: SummaryText = %q, true; want false", name, got)
		}
	}
}

// TestSummarize_StoredSummaryRolls is #144's flow: an app stores the
// summary between turns, rebuilds head + SummaryMessage + newer turns +
// the new question, and compacts outside Run. The stored summary is sent
// in full as the one to update, not truncated as an ordinary turn, and the
// new summary is read back with SummaryText.
func TestSummarize_StoredSummaryRolls(t *testing.T) {
	stored := "alpha=" + strings.Repeat("7", 50)
	msgs := []llms.Message{llms.UserText("q1"), compact.SummaryMessage(stored)}
	for _, q := range []string{"q5", "q6"} {
		msgs = append(msgs, llms.UserText(q), testllm.TextResponse("a-"+q).Message)
	}
	msgs = append(msgs, llms.UserText("NOW"))

	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("S2")}}
	c := mustSummarize(t, compact.SummarizeConfig{Client: fake, KeepTurns: 1, TruncateRunes: 10})
	res, err := c(context.Background(), toolloop.CompactInput{Reason: toolloop.CompactOverflow, Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	text := fake.Calls()[0].Messages[0].Content[0].Text
	if !strings.HasPrefix(text, "Existing summary. ") || !strings.Contains(text, stored) || !strings.Contains(text, "[user]\nq5") {
		t.Fatalf("summary request:\n%s", text)
	}
	if got := ids(res.Messages[2:]); got != "q6 assistant NOW" {
		t.Fatalf("kept after the summary = %s", got)
	}
	if got, ok := compact.SummaryText(res.Messages[1]); !ok || got != "S2" {
		t.Fatalf("SummaryText(result[1]) = %q, %v; want S2", got, ok)
	}
}

// TestSummaryText_V1Format pins the stored format with a literal, apart
// from SummaryMessage: a summary an app stored under v1 must stay
// recognized by later v1 releases (ADR-0039).
func TestSummaryText_V1Format(t *testing.T) {
	const v1 = "Summary of the earlier part of this conversation (older turns were removed to fit the context window):\n\nfacts"
	if got, ok := compact.SummaryText(llms.UserText(v1)); !ok || got != "facts" {
		t.Fatalf("SummaryText(v1 literal) = %q, %v; want facts, true", got, ok)
	}
	if got := compact.SummaryMessage("facts"); len(got.Content) != 1 || got.Content[0].Text != v1 || got.Role != llms.RoleUser {
		t.Fatalf("SummaryMessage(facts) = %+v, want the v1 literal", got)
	}
}

// summaryFirst is a transcript an app rebuilt from a stored summary and
// the newer turns, without the original first message.
func summaryFirst() []llms.Message {
	msgs := []llms.Message{compact.SummaryMessage("S1")}
	for _, q := range []string{"q2", "q3"} {
		msgs = append(msgs, llms.UserText(q), testllm.TextResponse("a-"+q).Message)
	}
	return append(msgs, llms.UserText("NOW"))
}

// TestSummarize_SummaryFirstRolls: a summary as the head is rolled in place,
// not kept beside a second summary (ADR-0039).
func TestSummarize_SummaryFirstRolls(t *testing.T) {
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("S2")}}
	res, err := mustSummarize(t, compact.SummarizeConfig{Client: fake, KeepTurns: 1})(context.Background(), overflow(summaryFirst()))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := compact.SummaryText(res.Messages[0]); !ok || got != "S2" {
		t.Fatalf("first message = %s, want summary S2", ids(res.Messages[:1]))
	}
	if got := ids(res.Messages[1:]); got != "q3 assistant NOW" {
		t.Fatalf("after the summary = %s", got)
	}
	if err := middleware.ValidateMessages(res.Messages); err != nil {
		t.Fatalf("result invalid: %v", err)
	}
	text := fake.Calls()[0].Messages[0].Content[0].Text
	if !strings.HasPrefix(text, "Existing summary. ") || !strings.Contains(text, "S1") || !strings.Contains(text, "[user]\nq2") {
		t.Fatalf("summary request:\n%s", text)
	}
}

// TestKeepLast_SummaryFirst: KeepLast keeps a leading summary as the head.
func TestKeepLast_SummaryFirst(t *testing.T) {
	res, err := mustKeepLast(t, compact.KeepLastConfig{Turns: 1})(context.Background(), overflow(summaryFirst()))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := compact.SummaryText(res.Messages[0]); !ok || got != "S1" {
		t.Fatalf("first message = %s, want summary S1", ids(res.Messages[:1]))
	}
	if got := ids(res.Messages[1:]); got != "q3 assistant NOW" {
		t.Fatalf("after the summary = %s", got)
	}
}

// TestSummaryFirst_OpensWithSummary: with a leading summary, both
// compactors return a non-empty transcript that opens with a summary, never
// an empty one or one opening with an assistant message.
func TestSummaryFirst_OpensWithSummary(t *testing.T) {
	s1 := compact.SummaryMessage("S1")
	toolRounds := append([]llms.Message{s1}, turn("c1", "r1")...)
	toolRounds = append(toolRounds, turn("c2", "r2")...)
	cases := []struct {
		name     string
		msgs     []llms.Message
		keep     int
		keepRest string // what KeepLast keeps after S1
		sumRest  string // what Summarize keeps after S2
	}{
		{"answered, keep 0", []llms.Message{s1, llms.UserText("q2"), testllm.TextResponse("a2").Message}, 0, "", ""},
		{"no prompt, keep 0", append([]llms.Message{s1}, turn("c1", "r1")...), 0, "", ""},
		{"tool rounds, keep 1", toolRounds, 1, "call:c2 result:c2", "call:c2 result:c2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := func(name string, msgs []llms.Message, summary, rest string) {
				t.Helper()
				if got, ok := compact.SummaryText(msgs[0]); !ok || got != summary {
					t.Fatalf("%s: first message = %s, want summary %s", name, ids(msgs[:1]), summary)
				}
				if got := ids(msgs[1:]); got != rest {
					t.Fatalf("%s: after the summary = %q, want %q", name, got, rest)
				}
				if err := middleware.ValidateMessages(msgs); err != nil {
					t.Fatalf("%s: result invalid: %v", name, err)
				}
			}
			res, err := mustKeepLast(t, compact.KeepLastConfig{Turns: tc.keep})(context.Background(), overflow(tc.msgs))
			if err != nil {
				t.Fatal(err)
			}
			check("KeepLast", res.Messages, "S1", tc.keepRest)
			fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("S2")}}
			res, err = mustSummarize(t, compact.SummarizeConfig{Client: fake, KeepTurns: tc.keep})(context.Background(), overflow(tc.msgs))
			if err != nil {
				t.Fatal(err)
			}
			check("Summarize", res.Messages, "S2", tc.sumRest)
			if text := fake.Calls()[0].Messages[0].Content[0].Text; !strings.Contains(text, "S1") {
				t.Fatalf("S1 not rolled:\n%s", text)
			}
		})
	}
}

// Media in dropped turns is not sent to the summarizer: each part becomes a
// placeholder naming its media type and size (ADR-0040).
func TestSummarize_MediaPlaceholder(t *testing.T) {
	msgs := []llms.Message{llms.UserText("task")}
	msgs = append(msgs, turn("t1", "one")...)
	msgs = append(msgs, llms.Message{Role: llms.RoleUser, Content: []llms.ContentPart{
		llms.Text("see attached"),
		llms.Image("image/png", []byte("12345")),
		llms.Document("application/pdf", []byte("%PDF-1.7")),
	}})
	msgs = append(msgs, turn("t2", "two")...)
	msgs = append(msgs, llms.UserText("nudge"))
	msgs = append(msgs, turn("t3", "three")...)

	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("s")}}
	c := mustSummarize(t, compact.SummarizeConfig{Client: fake, KeepTurns: 1})
	res, err := c(context.Background(), overflow(msgs))
	if err != nil {
		t.Fatal(err)
	}
	if err := middleware.ValidateMessages(res.Messages); err != nil {
		t.Fatalf("result invalid: %v", err)
	}
	req := fake.Calls()[0]
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type != llms.ContentText {
				t.Fatalf("summarizer request carries a %s part", p.Type)
			}
		}
	}
	text := req.Messages[0].Content[0].Text
	for _, want := range []string{
		"[user]\nsee attached",
		"[user attached an image (image/png, 5 bytes); not shown]",
		"[user attached a document (application/pdf, 8 bytes); not shown]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered history lacks %q:\n%s", want, text)
		}
	}
}

// TestSummarize_ToolResultPartsPlaceholder: a dropped tool result with Parts
// renders its text and a placeholder per media part (ADR-0041), and a long
// text part is truncated without cutting off the media line.
func TestSummarize_ToolResultPartsPlaceholder(t *testing.T) {
	call := testllm.ToolCallResponse("t1", "get_document", `{}`).Message
	msgs := []llms.Message{
		llms.UserText("task"),
		call,
		llms.ToolResultMessage(llms.ToolResult{ToolCallID: "t1", Parts: []llms.ContentPart{
			llms.Text(strings.Repeat("x", 50)),
			llms.Document("application/pdf", []byte("%PDF-1.7")),
		}}),
	}
	msgs = append(msgs, turn("t2", "two")...)
	msgs = append(msgs, turn("t3", "three")...)

	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("s")}}
	c := mustSummarize(t, compact.SummarizeConfig{Client: fake, KeepTurns: 1, TruncateRunes: 10})
	if _, err := c(context.Background(), overflow(msgs)); err != nil {
		t.Fatal(err)
	}
	text := fake.Calls()[0].Messages[0].Content[0].Text
	want := "[tool result from get_document]\n" + strings.Repeat("x", 10)
	if !strings.Contains(text, want) {
		t.Errorf("rendered history lacks truncated text %q:\n%s", want, text)
	}
	if !strings.Contains(text, "\n[tool returned a document (application/pdf, 8 bytes); not shown]") {
		t.Errorf("rendered history lacks the media placeholder:\n%s", text)
	}
}

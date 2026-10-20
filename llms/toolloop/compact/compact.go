// Package compact provides two ready-made toolloop.Compactor policies
// (ADR-0030): KeepLast, which drops older turns, and Summarize, which
// replaces them with a summary written by an app-supplied ChatClient.
// What to keep is application policy; apps with other needs write their own
// toolloop.Compactor, and may wrap these.
//
// Both compactors share one cut rule. The head (every message through the
// first user message, normally the task statement) is always kept. A turn
// starts at an assistant message and includes its tool-result message, plus
// any user messages immediately before it (its prompt), so a cut never
// separates a tool call from its result. User messages after the last turn
// (a prompt not yet answered) are always kept. The compacted transcript has
// every reasoning part removed (llms.WithoutReasoning, ADR-0021), since an
// edited history can invalidate the reasoning that follows the edit.
//
// Both act on every overflow (toolloop.CompactOverflow), and before a turn
// only when AtTokens is positive and the last response's InputTokens +
// OutputTokens reached it. With nothing to drop they return no change, so
// an overflow that compaction cannot help stays
// toolloop.OutcomeContextExceeded.
package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// KeepLastConfig configures KeepLast. Turns is the number of most recent
// turns kept after the head; zero keeps only the head (and any unanswered
// prompt). AtTokens is the before-turn threshold; zero compacts only on
// overflow.
type KeepLastConfig struct {
	Turns    int
	AtTokens int
}

// KeepLast returns a Compactor that keeps the head and the last cfg.Turns
// turns and drops the rest. It makes no provider calls. It returns an
// error for negative fields.
func KeepLast(cfg KeepLastConfig) (toolloop.Compactor, error) {
	if cfg.Turns < 0 || cfg.AtTokens < 0 {
		return nil, errors.New("compact: KeepLastConfig.Turns and AtTokens must not be negative")
	}
	return func(_ context.Context, in toolloop.CompactInput) (toolloop.CompactResult, error) {
		if !due(&in, cfg.AtTokens) {
			return toolloop.CompactResult{}, nil
		}
		head, cut := split(in.Messages, cfg.Turns)
		if cut <= head {
			return toolloop.CompactResult{}, nil
		}
		out := make([]llms.Message, 0, head+len(in.Messages)-cut)
		out = append(out, in.Messages[:head]...)
		out = append(out, in.Messages[cut:]...)
		return toolloop.CompactResult{Messages: llms.WithoutReasoning(out)}, nil
	}, nil
}

// DefaultSummaryPrompt is the system prompt Summarize uses when
// SummarizeConfig.Prompt is empty.
const DefaultSummaryPrompt = "You compress the history of an agent's tool-using session so it can continue " +
	"in a smaller context. Summarize the conversation you are given: keep the facts learned, " +
	"decisions made, results of tool calls that later work depends on, identifiers and values " +
	"needed to continue, and what remains open. If you are given an existing summary, " +
	"write an updated summary: everything in the existing one plus the new turns, since " +
	"yours replaces it. Omit pleasantries and anything superseded. " +
	"Write plain prose or terse bullet points, without preamble."

// summaryHeader introduces the summary in the compacted transcript.
const summaryHeader = "Summary of the earlier part of this conversation (older turns were removed to fit the context window):\n\n"

// defaultTruncateRunes is SummarizeConfig.TruncateRunes when zero.
const defaultTruncateRunes = 4000

// SummarizeConfig configures Summarize.
//
// Client writes the summary; it may be the loop's client or a different
// (cheaper) model. Its usage is reported as toolloop.Outcome.CompactionUsage,
// apart from the loop's TotalUsage. Prompt is its system prompt
// (DefaultSummaryPrompt when empty), and MaxTokens its output limit (zero
// leaves the adapter default). A summarizer that reasons by default can
// spend its whole MaxTokens on reasoning and return no text, which is an
// error naming the stop reason: give it room, or use a non-reasoning model. Purpose is passed through as the summary
// request's ChatRequest.Purpose, so the app can label it for metrics.
//
// KeepTurns is the number of most recent turns kept verbatim after the head;
// older turns are summarized. AtTokens is the before-turn threshold; zero
// compacts only on overflow.
//
// TruncateRunes caps each text, tool argument and tool result rendered for
// the summarizer, in runes: the turns being summarized are often the
// oversized ones, and sent whole would overflow the summarizer too. Zero
// uses 4000; negative means no limit.
type SummarizeConfig struct {
	Client        llms.ChatClient
	Prompt        string
	Purpose       llms.Purpose
	KeepTurns     int
	AtTokens      int
	MaxTokens     int
	TruncateRunes int
}

// Summarize returns a Compactor that keeps the head and the last
// cfg.KeepTurns turns, and replaces the turns before them with one user
// message carrying a summary from cfg.Client. Summaries roll: a previous
// summary is among the dropped messages on the next compaction, so it is
// sent in full as the existing summary to update, apart from the new turns,
// and the default prompt asks for an updated summary. The dropped turns are sent to
// the summarizer as plain text (roles, tool names, arguments and results),
// not as structured messages, so the request needs no tool definitions and
// no provider-bound tool-call IDs, signatures or reasoning. A summarizer
// error, or an empty summary, is returned as the Compactor's error, so the
// loop stops with toolloop.OutcomeCompactionError; an app that wants a
// fallback wraps it (for example, with KeepLast). It returns an error for a
// nil client or negative counts.
func Summarize(cfg SummarizeConfig) (toolloop.Compactor, error) {
	if cfg.Client == nil {
		return nil, errors.New("compact: SummarizeConfig.Client is required")
	}
	if cfg.KeepTurns < 0 || cfg.AtTokens < 0 || cfg.MaxTokens < 0 {
		return nil, errors.New("compact: SummarizeConfig.KeepTurns, AtTokens and MaxTokens must not be negative")
	}
	prompt := cfg.Prompt
	if prompt == "" {
		prompt = DefaultSummaryPrompt
	}
	limit := cfg.TruncateRunes
	if limit == 0 {
		limit = defaultTruncateRunes
	}
	return func(ctx context.Context, in toolloop.CompactInput) (toolloop.CompactResult, error) {
		if !due(&in, cfg.AtTokens) {
			return toolloop.CompactResult{}, nil
		}
		head, cut := split(in.Messages, cfg.KeepTurns)
		if cut <= head {
			return toolloop.CompactResult{}, nil
		}
		resp, err := cfg.Client.Complete(ctx, llms.ChatRequest{
			System:    []llms.ContentPart{llms.Text(prompt)},
			Messages:  []llms.Message{llms.UserText(render(in.Messages[head:cut], limit))},
			MaxTokens: cfg.MaxTokens,
			Purpose:   cfg.Purpose,
		})
		if err != nil {
			return toolloop.CompactResult{}, fmt.Errorf("compact: summarizer: %w", err)
		}
		summary := strings.TrimSpace(resp.Text)
		if summary == "" {
			return toolloop.CompactResult{Usage: resp.Usage}, fmt.Errorf("compact: summarizer returned no text (stop reason %q)", resp.StopReason)
		}
		out := make([]llms.Message, 0, head+1+len(in.Messages)-cut)
		out = append(out, in.Messages[:head]...)
		out = append(out, llms.UserText(summaryHeader+summary))
		out = append(out, in.Messages[cut:]...)
		return toolloop.CompactResult{Messages: llms.WithoutReasoning(out), Usage: resp.Usage}, nil
	}, nil
}

// due reports whether a compactor should act: always on overflow, and
// before a turn once the last response's token count reached atTokens.
func due(in *toolloop.CompactInput, atTokens int) bool {
	if in.Reason == toolloop.CompactOverflow {
		return true
	}
	return atTokens > 0 && in.LastUsage.InputTokens+in.LastUsage.OutputTokens >= atTokens
}

// split returns the head length and the index of the first message kept
// after it when keeping the last keep turns. Messages [head, cut) are the
// ones to drop; cut <= head means there is nothing to drop.
func split(msgs []llms.Message, keep int) (head, cut int) {
	head = len(msgs)
	for i := range msgs {
		if msgs[i].Role == llms.RoleUser {
			head = i + 1
			break
		}
	}
	var starts []int
	for i := head; i < len(msgs); i++ {
		if msgs[i].Role != llms.RoleAssistant {
			continue
		}
		start := i
		for start > head && msgs[start-1].Role == llms.RoleUser {
			start--
		}
		starts = append(starts, start)
	}
	if keep > 0 {
		if len(starts) <= keep {
			return head, head
		}
		return head, starts[len(starts)-keep]
	}
	// Keep no turns: only the unanswered prompt after the last turn, if any.
	cut = len(msgs)
	for cut > head && msgs[cut-1].Role == llms.RoleUser {
		cut--
	}
	return head, cut
}

// render builds the summarizer's request text from the dropped messages.
// Turns are written as plain text, each piece truncated to limit runes (no
// limit when negative); reasoning is omitted. A previous Summarize result is
// set apart as the existing summary to update, in full: it is already
// bounded by the summarizer's MaxTokens, and the new summary replaces it,
// so whatever is cut or overlooked is lost.
func render(msgs []llms.Message, limit int) string {
	var prior []string
	names := map[string]string{}
	var b strings.Builder
	for i := range msgs {
		for j := range msgs[i].Content {
			p := &msgs[i].Content[j]
			switch {
			case p.Type == llms.ContentText && msgs[i].Role == llms.RoleUser && strings.HasPrefix(p.Text, summaryHeader):
				prior = append(prior, strings.TrimPrefix(p.Text, summaryHeader))
			case p.Type == llms.ContentText && p.Text != "":
				fmt.Fprintf(&b, "[%s]\n%s\n\n", msgs[i].Role, truncate(p.Text, limit))
			case p.Type == llms.ContentToolCall && p.ToolCall != nil:
				names[p.ToolCall.ID] = p.ToolCall.Name
				fmt.Fprintf(&b, "[assistant called tool %s]\n%s\n\n", p.ToolCall.Name, truncate(string(p.ToolCall.Parameters), limit))
			case p.Type == llms.ContentToolResult && p.ToolResult != nil:
				label := "result"
				if p.ToolResult.IsError {
					label = "error"
				}
				fmt.Fprintf(&b, "[tool %s from %s]\n%s\n\n", label, names[p.ToolResult.ToolCallID], truncate(p.ToolResult.Content, limit))
			}
		}
	}
	turns := strings.TrimSpace(b.String())
	if len(prior) == 0 {
		return turns
	}
	return existingSummaryLabel + strings.Join(prior, "\n\n") + newTurnsLabel + turns
}

// existingSummaryLabel and newTurnsLabel frame an incremental summary.
const (
	existingSummaryLabel = "Existing summary. Your summary replaces it, so it must keep every fact below:\n\n"
	newTurnsLabel        = "\n\nNew turns to add to the summary:\n\n"
)

// truncate shortens s to limit runes, marking the cut. A negative limit
// means no limit.
func truncate(s string, limit int) string {
	if limit < 0 {
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i] + fmt.Sprintf(" …[truncated %d bytes]", len(s)-i)
		}
		n++
	}
	return s
}

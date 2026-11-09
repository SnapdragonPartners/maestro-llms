// Package compact provides two ready-made toolloop.Compactor policies
// (ADR-0030): KeepLast, which drops older turns, and Summarize, which
// replaces them with a summary written by an app-supplied ChatClient.
// What to keep is application policy; apps with other needs write their own
// toolloop.Compactor, and may wrap these.
//
// Both compactors share one cut rule. The head (every message through the
// first user message, normally the task statement) is never dropped, so in
// a long chat the first message is kept verbatim and never summarized,
// unless it is itself a summary: when the first user message is a summary
// (a transcript rebuilt as summary plus newer turns), KeepLast keeps it as
// the head, and Summarize, whenever it compacts, rolls it and puts the new
// summary in its place (ADR-0039). A turn
// starts at an assistant message and includes its tool-result message, plus
// any user messages immediately before it (its prompt), so a cut never
// separates a tool call from its result. Two prompts are always kept
// (ADR-0038): user messages after the last turn (a prompt not yet
// answered), and the in-flight prompt, the last user messages before the
// assistant turns that are still answering them (the transcript does not
// end with an assistant message). A chat app that sends the conversation so
// far plus a new question has its new question pinned this way, while the
// older questions and answers are dropped or summarized first; in an agent
// loop the in-flight prompt is usually the head. The compacted transcript has
// every reasoning part removed (llms.WithoutReasoning, ADR-0021), since an
// edited history can invalidate the reasoning that follows the edit.
//
// Both act on every overflow (toolloop.CompactOverflow), and before a turn
// only when AtTokens is positive and the last response's InputTokens +
// OutputTokens reached it. With nothing to drop they return no change, so
// an overflow that compaction cannot help stays
// toolloop.OutcomeContextExceeded.
//
// The compactors also work on stored history outside toolloop.Run, for
// example before a chat turn. To compact now, regardless of AtTokens, call
// one with CompactInput.Reason set to toolloop.CompactOverflow. An app that
// stores a Summarize result between turns finds the summary with
// SummaryText and, when it rebuilds the transcript, puts it back as
// SummaryMessage, so the next compaction rolls it (ADR-0039).
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
// turns kept after the head; zero keeps only the head (and the unanswered
// and in-flight prompts). AtTokens is the before-turn threshold; zero
// compacts only on overflow.
type KeepLastConfig struct {
	Turns    int
	AtTokens int
}

// KeepLast returns a Compactor that keeps the head, the in-flight prompt and
// the last cfg.Turns turns, and drops the rest. It makes no provider calls.
// It returns an error for negative fields.
func KeepLast(cfg KeepLastConfig) (toolloop.Compactor, error) {
	if cfg.Turns < 0 || cfg.AtTokens < 0 {
		return nil, errors.New("compact: KeepLastConfig.Turns and AtTokens must not be negative")
	}
	return func(_ context.Context, in toolloop.CompactInput) (toolloop.CompactResult, error) {
		if !due(&in, cfg.AtTokens) {
			return toolloop.CompactResult{}, nil
		}
		s := split(in.Messages, cfg.Turns)
		if !s.drops() {
			return toolloop.CompactResult{}, nil
		}
		return toolloop.CompactResult{Messages: llms.WithoutReasoning(s.kept(in.Messages, nil))}, nil
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

// summaryHeader introduces the summary in the compacted transcript. Apps
// store summary messages, so it is fixed for v1 (ADR-0039).
const summaryHeader = "Summary of the earlier part of this conversation (older turns were removed to fit the context window):\n\n"

// SummaryMessage returns the message Summarize puts in place of the turns
// it removes, carrying summary. Summarize recognizes it (as SummaryText
// does) and rolls it on the next compaction: it is sent in full as the
// existing summary to update, rather than truncated as an ordinary turn,
// and it is never taken for the in-flight prompt.
func SummaryMessage(summary string) llms.Message {
	return llms.UserText(summaryHeader + summary)
}

// SummaryText reports whether msg is a summary message (a user message
// whose only part is text in the SummaryMessage format), and returns its
// summary.
func SummaryText(msg llms.Message) (string, bool) {
	if msg.Role != llms.RoleUser || len(msg.Content) != 1 || msg.Content[0].Type != llms.ContentText {
		return "", false
	}
	return strings.CutPrefix(msg.Content[0].Text, summaryHeader)
}

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

// Summarize returns a Compactor that keeps the head, the in-flight prompt
// and the last cfg.KeepTurns turns, and replaces the other turns with one
// user message, right after the head, carrying a summary from cfg.Client
// (built by SummaryMessage).
// Summaries roll: a previous summary is among the dropped messages on the
// next compaction, so it is sent in full as the existing summary to update,
// apart from the new turns, and the default prompt asks for an updated
// summary. When the head's first user message is itself a summary (a
// transcript rebuilt from a stored one), it is rolled the same way: the new
// summary replaces it in place instead of being added beside it (ADR-0039). The dropped turns are sent to the summarizer as plain text
// (roles, tool names, arguments and results),
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
		s := split(in.Messages, cfg.KeepTurns)
		if !s.drops() {
			return toolloop.CompactResult{}, nil
		}
		if isSummary(&in.Messages[s.head-1]) {
			// The head is a summary (a transcript rebuilt from a stored
			// one): roll it in place rather than add a second (ADR-0039).
			s.head--
		}
		resp, err := cfg.Client.Complete(ctx, llms.ChatRequest{
			System:    []llms.ContentPart{llms.Text(prompt)},
			Messages:  []llms.Message{llms.UserText(render(s.dropped(in.Messages), limit))},
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
		out := s.kept(in.Messages, []llms.Message{SummaryMessage(summary)})
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

// span is the cut rule applied to a transcript. msgs[:head] is the head and
// msgs[cut:] the kept turns (and unanswered prompt). msgs[pin:pinEnd] is the
// in-flight prompt when it falls in [head, cut), and empty otherwise. The
// other messages in [head, cut) are dropped.
type span struct{ head, pin, pinEnd, cut int }

// drops reports whether the span drops any message.
func (s span) drops() bool { return s.cut-s.head > s.pinEnd-s.pin }

// dropped returns the dropped messages, in order.
func (s span) dropped(msgs []llms.Message) []llms.Message {
	out := make([]llms.Message, 0, s.cut-s.head-(s.pinEnd-s.pin))
	out = append(out, msgs[s.head:s.pin]...)
	return append(out, msgs[s.pinEnd:s.cut]...)
}

// kept returns the compacted transcript: the head, then insert (a summary),
// then the in-flight prompt, then the kept turns. Summarize shortens the
// head by one first when it ends in a summary, so the new summary replaces
// it (ADR-0039).
func (s span) kept(msgs, insert []llms.Message) []llms.Message {
	out := make([]llms.Message, 0, s.head+len(insert)+s.pinEnd-s.pin+len(msgs)-s.cut)
	out = append(out, msgs[:s.head]...)
	out = append(out, insert...)
	out = append(out, msgs[s.pin:s.pinEnd]...)
	return append(out, msgs[s.cut:]...)
}

// split applies the cut rule, keeping the last keep turns.
func split(msgs []llms.Message, keep int) span {
	head := headLen(msgs)
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
	var cut int
	switch {
	case keep > 0 && len(starts) <= keep:
		cut = head
	case keep > 0:
		cut = starts[len(starts)-keep]
	default:
		// Keep no turns: only the unanswered prompt after the last turn, if any.
		cut = len(msgs)
		for cut > head && msgs[cut-1].Role == llms.RoleUser {
			cut--
		}
	}
	s := span{head: head, pin: cut, pinEnd: cut, cut: cut}
	if pin, end := inFlightPrompt(msgs, head); pin < end && end <= cut {
		s.pin, s.pinEnd = pin, end
	}
	return s
}

// headLen returns the head's length: every message through the first user
// message.
func headLen(msgs []llms.Message) int {
	for i := range msgs {
		if msgs[i].Role == llms.RoleUser {
			return i + 1
		}
	}
	return len(msgs)
}

// inFlightPrompt returns the in-flight prompt msgs[pin:end]: the last run of
// user messages after the head, other than summaries, when the transcript
// does not end with an assistant message (so the turns after the prompt are
// still answering it). pin == end means there is none.
func inFlightPrompt(msgs []llms.Message, head int) (pin, end int) {
	if len(msgs) == 0 || msgs[len(msgs)-1].Role == llms.RoleAssistant {
		return head, head
	}
	end = len(msgs)
	for end > head && (msgs[end-1].Role != llms.RoleUser || isSummary(&msgs[end-1])) {
		end--
	}
	pin = end
	for pin > head && msgs[pin-1].Role == llms.RoleUser && !isSummary(&msgs[pin-1]) {
		pin--
	}
	return pin, end
}

// isSummary reports whether msg is a summary message.
func isSummary(msg *llms.Message) bool {
	_, ok := SummaryText(*msg)
	return ok
}

// mediaNoun names a media part type for the summarizer's placeholder.
func mediaNoun(t llms.ContentPartType) string {
	if t == llms.ContentImage {
		return "an image"
	}
	return "a document"
}

// mediaPlaceholder describes a media part for the summarizer without its
// bytes, for example "an image (image/png, 5 bytes); not shown".
func mediaPlaceholder(p *llms.ContentPart) string {
	return fmt.Sprintf("%s (%s, %d bytes); not shown", mediaNoun(p.Type), p.Media.MediaType, len(p.Media.Data))
}

// resultText renders a tool result for the summarizer: Content, or each of
// its Parts on its own line, text truncated to limit runes and media as a
// placeholder (ADR-0041), so a long text part cannot cut off the media lines.
func resultText(r *llms.ToolResult, limit int) string {
	if len(r.Parts) == 0 {
		return truncate(r.Content, limit)
	}
	lines := make([]string, 0, len(r.Parts))
	for i := range r.Parts {
		p := &r.Parts[i]
		switch {
		case p.Type == llms.ContentText && p.Text != "":
			lines = append(lines, truncate(p.Text, limit))
		case (p.Type == llms.ContentImage || p.Type == llms.ContentDocument) && p.Media != nil:
			lines = append(lines, "[tool returned "+mediaPlaceholder(p)+"]")
		}
	}
	return strings.Join(lines, "\n")
}

// render builds the summarizer's request text from the dropped messages.
// Turns are written as plain text, each piece truncated to limit runes (no
// limit when negative); reasoning is omitted, and an image or document is
// replaced by a one-line placeholder naming its media type and size, also
// inside a tool result's Parts (ADR-0041). A previous Summarize result is
// set apart as the existing summary to update, in full: it is already
// bounded by the summarizer's MaxTokens, and the new summary replaces it,
// so whatever is cut or overlooked is lost.
func render(msgs []llms.Message, limit int) string {
	var prior []string
	names := map[string]string{}
	var b strings.Builder
	for i := range msgs {
		if summary, ok := SummaryText(msgs[i]); ok {
			prior = append(prior, summary)
			continue
		}
		for j := range msgs[i].Content {
			p := &msgs[i].Content[j]
			switch {
			case p.Type == llms.ContentText && p.Text != "":
				fmt.Fprintf(&b, "[%s]\n%s\n\n", msgs[i].Role, truncate(p.Text, limit))
			case p.Type == llms.ContentToolCall && p.ToolCall != nil:
				names[p.ToolCall.ID] = p.ToolCall.Name
				fmt.Fprintf(&b, "[assistant called tool %s]\n%s\n\n", p.ToolCall.Name, truncate(string(p.ToolCall.Parameters), limit))
			case (p.Type == llms.ContentImage || p.Type == llms.ContentDocument) && p.Media != nil:
				// Media is not sent to the summarizer (ADR-0040): it may
				// not accept it, and resending it defeats compaction. A
				// placeholder keeps the fact that something was attached.
				fmt.Fprintf(&b, "[%s attached %s]\n\n", msgs[i].Role, mediaPlaceholder(p))
			case p.Type == llms.ContentToolResult && p.ToolResult != nil:
				label := "result"
				if p.ToolResult.IsError {
					label = "error"
				}
				fmt.Fprintf(&b, "[tool %s from %s]\n%s\n\n", label, names[p.ToolResult.ToolCallID],
					resultText(p.ToolResult, limit))
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

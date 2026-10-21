//go:build integration

package anthropic_test

import (
	"os"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
)

func newAnthropicToolLoopClient(t *testing.T) llms.ChatClient {
	t.Helper()
	c, err := anthropic.New(anthropic.WithAPIKey(anthropicAPIKey(t)), anthropic.WithModel(anthropicModel()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// Live toolloop scenarios shared across providers (issue #51); see
// llms/internal/livetest.
func TestIntegrationAnthropicToolLoopConvergence(t *testing.T) {
	livetest.ToolLoopConvergence(t, newAnthropicToolLoopClient(t))
}

func TestIntegrationAnthropicLongToolConversation(t *testing.T) {
	livetest.LongToolConversation(t, newAnthropicToolLoopClient(t))
}

func TestIntegrationAnthropicToolLoopNoTools(t *testing.T) {
	livetest.ToolLoopNoTools(t, newAnthropicToolLoopClient(t))
}

// ADR-0030: Summarize carries facts across compacted turns.
func TestIntegrationAnthropicCompactSummarize(t *testing.T) {
	livetest.CompactSummarize(t, newAnthropicToolLoopClient(t), nil)
}

// reasoningModel is the model for the ADR-0021 replay test: one that thinks
// by default and runs Anthropic's preserved-thinking check.
func reasoningModel() string {
	if m := os.Getenv("ANTHROPIC_REASONING_MODEL"); m != "" {
		return m
	}
	return "claude-opus-5-5"
}

// newStrictReasoningClient forces preserved-thinking enforcement
// (prefix_mismatch_behavior "error", which needs the beta header) so a
// replayed thinking block that doesn't match its conversation fails the
// request instead of being silently dropped (ADR-0021). Display "summarized"
// only makes Reasoning.Text readable in the log; replay doesn't depend on it.
func newStrictReasoningClient(t *testing.T) llms.ChatClient {
	t.Helper()
	c, err := anthropic.New(
		anthropic.WithAPIKey(anthropicAPIKey(t)),
		anthropic.WithModel(reasoningModel()),
		anthropic.WithRequestOptions(
			option.WithHeader("anthropic-beta", "thinking-binding-controls-2026-08-01"),
			// Effort max: adaptive thinking may skip thinking on an easy
			// step, and the test needs thinking blocks to replay.
			option.WithJSONSet("output_config", map[string]any{"effort": "max"}),
			option.WithJSONSet("thinking", map[string]any{
				"type":          "adaptive",
				"display":       "summarized",
				"block_binding": map[string]any{"prefix_mismatch_behavior": "error"},
			}),
		),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestIntegrationAnthropicReasoningReplay(t *testing.T) {
	livetest.ReasoningReplay(t, newStrictReasoningClient(t))
}

// TestIntegrationAnthropicReasoningCompaction runs the ADR-0030 summarize
// scenario under strict preserved-thinking enforcement: every compacted
// transcript drops earlier turns, and must be accepted because the
// compactor strips all reasoning (ADR-0021). The summarizer is the default
// tool-loop model: at effort max the reasoning client spends the summary's
// whole output budget on thinking.
func TestIntegrationAnthropicReasoningCompaction(t *testing.T) {
	livetest.CompactSummarize(t, newStrictReasoningClient(t), newAnthropicToolLoopClient(t))
}

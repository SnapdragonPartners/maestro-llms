//go:build integration

package openai_test

import (
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
)

func newOpenAIToolLoopClient(t *testing.T) llms.ChatClient {
	t.Helper()
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	c, err := openai.New(openai.WithAPIKey(key), openai.WithModel(openaiChatModel()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// Live toolloop scenarios shared across providers (issue #51); see
// llms/internal/livetest.
func TestIntegrationOpenAIToolLoopConvergence(t *testing.T) {
	livetest.ToolLoopConvergence(t, newOpenAIToolLoopClient(t))
}

func TestIntegrationOpenAILongToolConversation(t *testing.T) {
	livetest.LongToolConversation(t, newOpenAIToolLoopClient(t))
}

func TestIntegrationOpenAIToolLoopNoTools(t *testing.T) {
	livetest.ToolLoopNoTools(t, newOpenAIToolLoopClient(t))
}

// ADR-0030: Summarize carries facts across compacted turns. The summarizer
// is a separate, stronger model (OPENAI_SUMMARY_MODEL, default
// gpt-4.1-mini): gpt-4o-mini's rolling summaries dropped an earlier value in
// about one run in three, a model limit rather than a toolkit one. This
// also exercises a summarizer that differs from the loop model.
func TestIntegrationOpenAICompactSummarize(t *testing.T) {
	loop := newOpenAIToolLoopClient(t)
	model := os.Getenv("OPENAI_SUMMARY_MODEL")
	if model == "" {
		model = "gpt-4.1-mini"
	}
	summarizer, err := openai.New(openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")), openai.WithModel(model))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.CompactSummarize(t, loop, summarizer)
}

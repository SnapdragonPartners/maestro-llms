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
	c, err := openai.NewChat(openai.WithAPIKey(key), openai.WithModel(openaiChatModel()))
	if err != nil {
		t.Fatalf("NewChat: %v", err)
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

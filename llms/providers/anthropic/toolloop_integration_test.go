//go:build integration

package anthropic_test

import (
	"testing"

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

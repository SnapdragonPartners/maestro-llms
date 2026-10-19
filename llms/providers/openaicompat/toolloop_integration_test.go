//go:build integration

package openaicompat_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

func newVLLMToolLoopClient(t *testing.T) llms.ChatClient {
	t.Helper()
	c, _ := newVLLMOrSkip(t)
	return c
}

// Live toolloop scenarios shared across providers (issue #51); see
// llms/internal/livetest.
func TestIntegrationVLLMToolLoopConvergence(t *testing.T) {
	livetest.ToolLoopConvergence(t, newVLLMToolLoopClient(t))
}

func TestIntegrationVLLMLongToolConversation(t *testing.T) {
	livetest.LongToolConversation(t, newVLLMToolLoopClient(t))
}

func TestIntegrationVLLMToolLoopNoTools(t *testing.T) {
	livetest.ToolLoopNoTools(t, newVLLMToolLoopClient(t))
}

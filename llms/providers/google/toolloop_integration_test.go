//go:build integration

package google_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
)

func newGoogleToolLoopClient(t *testing.T) llms.ChatClient {
	t.Helper()
	c, err := google.New(google.WithAPIKey(apiKey(t)), google.WithModel(model()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// Live toolloop scenarios shared across providers (issue #51); see
// llms/internal/livetest.
func TestIntegrationGoogleToolLoopConvergence(t *testing.T) {
	livetest.ToolLoopConvergence(t, newGoogleToolLoopClient(t))
}

func TestIntegrationGoogleLongToolConversation(t *testing.T) {
	livetest.LongToolConversation(t, newGoogleToolLoopClient(t))
}

func TestIntegrationGoogleToolLoopNoTools(t *testing.T) {
	livetest.ToolLoopNoTools(t, newGoogleToolLoopClient(t))
}

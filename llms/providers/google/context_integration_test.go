//go:build integration

package google_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
)

// TestIntegrationGoogleContextOverflow is the ADR-0026 live trigger. The
// model is fixed (not GOOGLE_MODEL) because the prompt is sized to its
// 1,048,576-token window: 1.1M tokens (~6.6 MB) is rejected unbilled.
func TestIntegrationGoogleContextOverflow(t *testing.T) {
	c, err := google.New(google.WithAPIKey(apiKey(t)), google.WithModel("gemini-2.5-flash"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.ContextOverflow(t, c, 1_100_000)
}

// TestIntegrationGoogleCompactOnOverflow is the ADR-0030 live overflow
// scenario, sized like the ADR-0026 trigger.
func TestIntegrationGoogleCompactOnOverflow(t *testing.T) {
	c, err := google.New(google.WithAPIKey(apiKey(t)), google.WithModel("gemini-2.5-flash"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	livetest.CompactOnOverflow(t, c, 1_100_000)
}

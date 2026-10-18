//go:build integration

package anthropic_test

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

func TestIntegrationAnthropicProviderParams(t *testing.T) {
	livetest.ProviderParamsOverride(t, newAnthropic(t, anthropicModel()), map[string]any{"max_tokens": 16})
}

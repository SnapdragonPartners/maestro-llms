package livetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// ContextOverflow is the ADR-0026 live trigger: it sends a single prompt of
// about words tokens ("hello " tokenizes as one token on every provider
// tested), which the caller sizes above the model's context window, through
// toolloop.Run. The provider must reject it with an error the adapter
// classifies as llms.ErrorKindContextLength, so the loop returns
// OutcomeContextExceeded. Retry middleware is applied, and must not retry.
//
// Providers reject an over-long prompt before inference, so the call is not
// billed; a prompt that fits would be, so size words for the fixed model the
// caller passes (not an env-overridable one). The provider's message is
// logged so a wording change is visible when the matcher stops matching.
func ContextOverflow(t *testing.T, c llms.ChatClient, words int) {
	t.Helper()
	if testing.Short() {
		t.Skip("live context overflow skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	out := toolloop.Run(ctx, toolloop.Config{
		Client: withRetry(c),
		Request: llms.ChatRequest{
			Messages:  []llms.Message{llms.UserText(strings.Repeat("hello ", words))},
			MaxTokens: 16,
		},
	})
	var pe *llms.ProviderError
	if errors.As(out.Err, &pe) {
		t.Logf("provider error: status=%d kind=%s message=%q", pe.StatusCode, pe.Kind, pe.Message)
	}
	if out.Kind != toolloop.OutcomeContextExceeded {
		t.Fatalf("kind = %q, want context_exceeded; err=%v", out.Kind, out.Err)
	}
	if pe == nil || pe.Kind != llms.ErrorKindContextLength || llms.Retryable(out.Err) {
		t.Fatalf("Err = %v, want a non-retryable context_length *llms.ProviderError", out.Err)
	}
	if len(out.Messages) != 1 {
		t.Errorf("Messages has %d entries, want the 1 message sent", len(out.Messages))
	}
}

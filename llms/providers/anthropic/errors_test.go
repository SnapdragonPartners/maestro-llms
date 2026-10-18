package anthropic

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// TestContextOverflowClassification feeds real captured error bodies
// (ADR-0026) through Complete: overflow is context_length on both the direct
// API and Vertex, and another invalid_request_error stays bad_request.
func TestContextOverflowClassification(t *testing.T) {
	cases := []struct {
		fixture string
		want    llms.ErrorKind
	}{
		{"testdata/prompt_too_long.json", llms.ErrorKindContextLength},
		{"testdata/prompt_too_long_vertex.json", llms.ErrorKindContextLength},
		{"testdata/max_tokens_too_large.json", llms.ErrorKindBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			body, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			c := newClient(t, respondJSON(t, 400, string(body), nil))
			_, err = c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
			var pe *llms.ProviderError
			if !errors.As(err, &pe) || pe.Kind != tc.want || pe.StatusCode != 400 {
				t.Fatalf("err = %v, want %q with status 400", err, tc.want)
			}
			if llms.Retryable(err) {
				t.Error("must not be retryable")
			}
		})
	}
}

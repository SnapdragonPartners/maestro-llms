package google

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// TestContextOverflowClassification feeds real captured error bodies
// (ADR-0026) through Complete: both backends' overflow wordings are
// context_length, and another INVALID_ARGUMENT stays bad_request.
func TestContextOverflowClassification(t *testing.T) {
	cases := []struct {
		fixture string
		want    llms.ErrorKind
	}{
		{"testdata/gemini_input_token_count.json", llms.ErrorKindContextLength},
		{"testdata/vertex_input_token_count.json", llms.ErrorKindContextLength},
		{"testdata/gemini_bad_temperature.json", llms.ErrorKindBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			body, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			c := newClient(t, jsonHandler(t, 400, string(body)))
			_, err = c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
			var pe *llms.ProviderError
			if !errors.As(err, &pe) || pe.Kind != tc.want || pe.StatusCode != 400 {
				t.Fatalf("err = %v, want %q with status 400", err, tc.want)
			}
		})
	}
}

// TestContextOverflowLegacyWording: the documented older form with the
// count in parentheses still matches.
func TestContextOverflowLegacyWording(t *testing.T) {
	body := `{"error":{"code":400,"message":"The input token count (1100017) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`
	c := newClient(t, jsonHandler(t, 400, body))
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindContextLength {
		t.Fatalf("err = %v, want context_length", err)
	}
}

package openaicompat

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// TestContextOverflowClassification feeds real captured LunaRoute error
// bodies (ADR-0026) through Complete: the structured code (DeepSeek) and the
// message-only form (GLM) are both context_length; another 400 from the same
// gateway, whose code is an integer, stays bad_request.
func TestContextOverflowClassification(t *testing.T) {
	cases := []struct {
		fixture string
		want    llms.ErrorKind
	}{
		{"testdata/lunaroute_deepseek_context_length.json", llms.ErrorKindContextLength},
		{"testdata/lunaroute_glm_context_length.json", llms.ErrorKindContextLength},
		{"testdata/lunaroute_bad_temperature.json", llms.ErrorKindBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			body, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			c := newClient(t, respondJSON(t, 400, string(body)))
			_, err = c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
			var pe *llms.ProviderError
			if !errors.As(err, &pe) || pe.Kind != tc.want || pe.StatusCode != 400 {
				t.Fatalf("err = %v, want %q with status 400", err, tc.want)
			}
		})
	}
}

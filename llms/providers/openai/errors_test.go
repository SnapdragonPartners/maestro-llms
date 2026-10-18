package openai

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestChatContextOverflowClassification feeds real captured Responses error
// bodies (ADR-0026) through Complete.
func TestChatContextOverflowClassification(t *testing.T) {
	cases := []struct {
		fixture string
		want    llms.ErrorKind
	}{
		{"testdata/context_length_exceeded.json", llms.ErrorKindContextLength},
		{"testdata/bad_temperature.json", llms.ErrorKindBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			c := newChat(t, jsonHandler(t, 400, readFixture(t, tc.fixture), nil))
			_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
			var pe *llms.ProviderError
			if !errors.As(err, &pe) || pe.Kind != tc.want || pe.StatusCode != 400 {
				t.Fatalf("err = %v, want %q with status 400", err, tc.want)
			}
		})
	}
}

// TestEmbedContextOverflowClassification: the embeddings endpoint sends the
// overflow with a null code, so it is matched on the message.
func TestEmbedContextOverflowClassification(t *testing.T) {
	c := newClient(t, jsonHandler(t, 400, readFixture(t, "testdata/embeddings_context_length.json"), nil))
	_, err := c.Embed(context.Background(), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{ID: "a", Text: "hi"}}})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindContextLength {
		t.Fatalf("err = %v, want context_length", err)
	}
}

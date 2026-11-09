package livetest

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// redPNG returns a solid red square PNG.
func redPNG(t *testing.T, size int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			img.Set(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// documentCodeWord is the word the generated documents carry.
const documentCodeWord = "PELICAN"

// codeWordPDF returns a minimal one-page PDF whose text states the code
// word, with a correct cross-reference table.
func codeWordPDF() []byte {
	content := fmt.Sprintf("BT /F1 18 Tf 72 720 Td (The code word is %s.) Tj ET", documentCodeWord)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

// completeMedia sends one user message and checks the answer contains want
// (case-insensitive). It also checks DefaultEstimator's input estimate is
// at least the reported input tokens (ADR-0040: media is counted biased
// high), when the provider reports usage.
func completeMedia(t *testing.T, c llms.ChatClient, want string, parts ...llms.ContentPart) {
	t.Helper()
	req := llms.ChatRequest{
		Messages:  []llms.Message{{Role: llms.RoleUser, Content: parts}},
		MaxTokens: 1024,
	}
	if err := middleware.ValidateMessages(req.Messages); err != nil {
		t.Fatalf("request invalid: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	resp, err := withRetry(c).Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	t.Logf("answer: %q usage: %+v", truncate(resp.Text, 200), resp.Usage)
	if !strings.Contains(strings.ToLower(resp.Text), strings.ToLower(want)) {
		t.Fatalf("answer %q does not contain %q", resp.Text, want)
	}
	est := middleware.DefaultEstimator{}.EstimateChat(req).InputTokens
	t.Logf("DefaultEstimator input estimate: %d, reported input: %d", est, resp.Usage.InputTokens)
	if resp.Usage.InputTokens > est {
		t.Fatalf("estimate %d below reported input tokens %d", est, resp.Usage.InputTokens)
	}
}

// ImageInput sends a solid red PNG and asks for its color (ADR-0040).
func ImageInput(t *testing.T, c llms.ChatClient) {
	t.Helper()
	completeMedia(t, c, "red",
		llms.Image("image/png", redPNG(t, 256)),
		llms.Text("What single color fills this image? Answer with one word."),
	)
}

// PDFInput sends a one-page PDF and asks for the code word in it (ADR-0040).
func PDFInput(t *testing.T, c llms.ChatClient) {
	t.Helper()
	completeMedia(t, c, documentCodeWord,
		llms.Document("application/pdf", codeWordPDF()),
		llms.Text("What is the code word in this document? Answer with the word only."),
	)
}

// TextDocumentInput sends a text/plain document and asks for the code word
// in it (ADR-0040; Anthropic maps it to a text-source document block).
func TextDocumentInput(t *testing.T, c llms.ChatClient) {
	t.Helper()
	completeMedia(t, c, documentCodeWord,
		llms.Document("text/plain", []byte("Meeting notes.\nThe code word is "+documentCodeWord+".\n")),
		llms.Text("What is the code word in this document? Answer with the word only."),
	)
}

// toolResultMedia runs a tool loop whose one tool returns parts (ADR-0041)
// and checks the final answer contains want (case-insensitive), proving the
// model read the media from the tool result. The client is wrapped in the
// validation middleware so the request shape is checked too. It also checks
// DefaultEstimator's input estimate for the last request is at least the
// reported input tokens, as completeMedia does.
func toolResultMedia(t *testing.T, c llms.ChatClient, want, prompt string, tool llms.ToolDefinition, parts ...llms.ContentPart) {
	t.Helper()
	calls := 0
	var tr trace
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	out := toolloop.Run(ctx, toolloop.Config{
		Client: middleware.ChainChat(c, middleware.ValidationChat(), middleware.RetryChat(middleware.DefaultRetryConfig())),
		Request: llms.ChatRequest{
			Purpose:   llms.PurposeChat,
			Messages:  []llms.Message{llms.UserText(prompt)},
			MaxTokens: maxTokens,
		},
		Tools: []toolloop.Tool{{
			Definition: tool,
			Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
				calls++
				return toolloop.ToolResult{Parts: parts}, nil
			},
		}},
		MaxIterations: 4,
		OnIteration:   tr.onIteration,
		OnToolCall:    tr.onToolCall,
	})
	tr.dump(t)
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind=%q iterations=%d err=%v final text=%q", out.Kind, out.Iterations, out.Err, out.Response.Text)
	}
	if calls == 0 {
		t.Fatalf("model answered without calling %s: %q", tool.Name, out.Response.Text)
	}
	t.Logf("answer: %q usage: %+v", truncate(out.Response.Text, 200), out.Response.Usage)
	if !strings.Contains(strings.ToLower(out.Response.Text), strings.ToLower(want)) {
		t.Fatalf("answer %q does not contain %q", out.Response.Text, want)
	}
	// The last request is the transcript before the final answer.
	last := llms.ChatRequest{Messages: out.Messages[:len(out.Messages)-1], Tools: []llms.ToolDefinition{tool}}
	est := middleware.DefaultEstimator{}.EstimateChat(last).InputTokens
	t.Logf("DefaultEstimator input estimate: %d, reported input: %d", est, out.Response.Usage.InputTokens)
	if out.Response.Usage.InputTokens > est {
		t.Fatalf("estimate %d below reported input tokens %d", est, out.Response.Usage.InputTokens)
	}
}

// ToolResultPDF has a get_document tool return a text part and a one-page
// PDF, and asks for the code word in the document (ADR-0041).
func ToolResultPDF(t *testing.T, c llms.ChatClient) {
	t.Helper()
	toolResultMedia(t, c, documentCodeWord,
		"Call get_document to fetch the document, then tell me the code word in it. Answer with the word only.",
		toolDef("get_document", "Returns the document as a PDF.", `{"type":"object","properties":{}}`),
		llms.Text("Here is the document."),
		llms.Document("application/pdf", codeWordPDF()),
	)
}

// ToolResultImage has a get_screenshot tool return a solid red PNG and asks
// for its color (ADR-0041).
func ToolResultImage(t *testing.T, c llms.ChatClient) {
	t.Helper()
	toolResultMedia(t, c, "red",
		"Call get_screenshot, then tell me the single color that fills the screenshot. Answer with one word.",
		toolDef("get_screenshot", "Returns a screenshot as a PNG image.", `{"type":"object","properties":{}}`),
		llms.Image("image/png", redPNG(t, 256)),
	)
}

// ToolResultCachedPDF is ToolResultPDF with CacheBreakpoint on the document
// part, proving the provider accepts a cache marker inside a tool result
// (Anthropic, ADR-0041). The document is below the minimum cacheable size,
// so no cache write is expected; the request must simply be accepted.
func ToolResultCachedPDF(t *testing.T, c llms.ChatClient) {
	t.Helper()
	doc := llms.Document("application/pdf", codeWordPDF())
	doc.CacheBreakpoint = true
	toolResultMedia(t, c, documentCodeWord,
		"Call get_document to fetch the document, then tell me the code word in it. Answer with the word only.",
		toolDef("get_document", "Returns the document as a PDF.", `{"type":"object","properties":{}}`),
		llms.Text("Here is the document."),
		doc,
	)
}

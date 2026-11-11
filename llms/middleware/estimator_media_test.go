package middleware

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestTokensForMedia(t *testing.T) {
	cases := []struct {
		name string
		part llms.ContentPart
		want int
	}{
		// Small images take the per-image floor (Gemini 3's 1,120).
		{"small image floor", llms.Image("image/png", pngOf(t, 200, 200)), minImageTokens},
		// 2000x1500: pixels/750 = 4000; patches 63*47 = 2961 * 1.2 = 3553.
		{"pixel formula", llms.Image("image/png", pngOf(t, 2000, 1500)), 4000},
		// 20x40000: pixels/750 = 1067; patches 1*1250 * 1.2 = 1500.
		{"patch formula", llms.Image("image/png", pngOf(t, 20, 40000)), 1500},
		// 8000x8000: pixels/750 = 85,334, capped.
		{"capped", llms.Image("image/png", pngOf(t, 8000, 8000)), unknownImageTokens},
		{"unreadable image", llms.Image("image/png", []byte("not a png")), unknownImageTokens},
		{"pdf pages", llms.Document("application/pdf", []byte("<</Type /Page>> <</Type /Page>> <</Type /Page>>")), documentOverheadTokens + 3*pdfPageTokens},
		{"pdf without visible pages", llms.Document("application/pdf", bytes.Repeat([]byte{0x9c}, 3000)), documentOverheadTokens + 1000},
		{"text document", llms.Document("text/plain", []byte(strings.Repeat("a", 300))), documentOverheadTokens + 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tokensForMedia(c.part.Type, c.part.Media); got != c.want {
				t.Fatalf("tokensForMedia = %d, want %d", got, c.want)
			}
		})
	}
}

// A media part used to cost only perPartOverhead; it now dominates the
// estimate, and many large ones sum without wrapping.
func TestEstimateChatCountsMedia(t *testing.T) {
	est := DefaultEstimator{}
	req := llms.ChatRequest{Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{
		llms.Text("q"), llms.Image("image/png", []byte("x")),
	}}}}
	u := est.EstimateChat(req)
	if u.InputTokens < unknownImageTokens {
		t.Fatalf("InputTokens = %d, want at least %d", u.InputTokens, unknownImageTokens)
	}

	huge := llms.Document("application/pdf", []byte("/Type /Pages /Count 9999999"))
	req.Messages = []llms.Message{
		{Role: llms.RoleUser, Content: []llms.ContentPart{huge, huge, huge}},
	}
	for range 300 {
		req.Messages = append(req.Messages, req.Messages[0])
	}
	u = est.EstimateChat(req)
	if u.InputTokens <= 0 {
		t.Fatalf("InputTokens = %d, wrapped", u.InputTokens)
	}
	if u.TotalTokensMax < u.InputTokens {
		t.Fatalf("TotalTokensMax = %d < InputTokens %d", u.TotalTokensMax, u.InputTokens)
	}
}

// Every estimator sum saturates (round-1 finding: the tool-schema loop
// wrapped after a saturated media subtotal on 32-bit targets).
func TestEstimatorSumsSaturate(t *testing.T) {
	if got := sum(math.MaxInt, 1, 1); got != math.MaxInt {
		t.Fatalf("sum = %d, want MaxInt", got)
	}
	tools := []llms.ToolDefinition{{Name: "f", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	if got := addTools(math.MaxInt-1, tools); got != math.MaxInt {
		t.Fatalf("addTools = %d, want MaxInt", got)
	}
	want := math.MaxInt / 3
	if math.MaxInt%3 != 0 {
		want++
	}
	if got := ceilDiv(math.MaxInt, 3); got != want {
		t.Fatalf("ceilDiv(MaxInt, 3) = %d, want %d", got, want)
	}
	for _, c := range []struct{ n, d, want int }{{0, 3, 0}, {1, 3, 1}, {3, 3, 1}, {4, 3, 2}} {
		if got := ceilDiv(c.n, c.d); got != c.want {
			t.Fatalf("ceilDiv(%d, %d) = %d, want %d", c.n, c.d, got, c.want)
		}
	}
}

// A huge PDF (which saturates on 32-bit) plus tools never wraps: the input
// estimate stays at least the media estimate.
func TestEstimateChatMediaPlusToolsNoWrap(t *testing.T) {
	huge := llms.Document("application/pdf", []byte("/Type /Pages /Count 9999999"))
	req := llms.ChatRequest{
		Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{huge, huge}}},
		Tools:    []llms.ToolDefinition{{Name: "f", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	u := DefaultEstimator{}.EstimateChat(req)
	if media := tokensForMedia(huge.Type, huge.Media); u.InputTokens < media {
		t.Fatalf("InputTokens = %d, below one document's %d (wrapped)", u.InputTokens, media)
	}
	if u.TotalTokensMax < u.InputTokens {
		t.Fatalf("TotalTokensMax = %d < InputTokens %d", u.TotalTokensMax, u.InputTokens)
	}
}

// TestEstimateToolResultParts: a tool result's Parts are counted like
// message parts (ADR-0041), so an image returned by a tool is not charged
// as an empty string.
func TestEstimateToolResultParts(t *testing.T) {
	img := llms.Image("image/png", pngOf(t, 2000, 1500))
	withParts := llms.ChatRequest{Messages: []llms.Message{llms.ToolResultMessage(llms.ToolResult{
		ToolCallID: "c1", Parts: []llms.ContentPart{llms.Text(strings.Repeat("a", 400)), img},
	})}}
	empty := llms.ChatRequest{Messages: []llms.Message{llms.ToolResultMessage(llms.ToolResult{ToolCallID: "c1"})}}
	got := DefaultEstimator{}.EstimateChat(withParts).InputTokens - DefaultEstimator{}.EstimateChat(empty).InputTokens
	want := 2*perPartOverhead + ceilDiv(400, charsPerToken) + 4000
	if got != want {
		t.Fatalf("tool result parts estimate = %d, want %d", got, want)
	}
}

// ADR-0045 item 10: an embedding input's Parts count like tool-result parts,
// beside Text; there is still no output and no bound.
func TestEstimateEmbeddingParts(t *testing.T) {
	img := llms.Image("image/png", pngOf(t, 2000, 1500))
	req := llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{
		{Text: strings.Repeat("b", 30)},
		{Parts: []llms.ContentPart{llms.Text(strings.Repeat("a", 400)), img}},
	}}
	got := DefaultEstimator{}.EstimateEmbeddings(req)
	want := ceilDiv(30, charsPerToken) + 2*perPartOverhead + ceilDiv(400, charsPerToken) + 4000
	if got.InputTokens != want || got.TotalTokensMax != want || got.OutputTokensMax != 0 || got.Requests != 1 {
		t.Fatalf("embedding estimate = %+v, want input/total %d", got, want)
	}
	if _, ok, err := (DefaultEstimator{}).BoundEmbeddings(llms.ModelRef{}, req); ok || err != nil {
		t.Fatalf("embeddings must stay unbounded (ADR-0043): ok=%v err=%v", ok, err)
	}
}

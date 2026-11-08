package middleware

import (
	"bytes"
	"image"
	"image/png"
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

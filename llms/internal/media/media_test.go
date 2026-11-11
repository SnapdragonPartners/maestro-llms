package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func TestCheck(t *testing.T) {
	cases := []struct {
		part    llms.ContentPart
		name    string
		want    string
		wantErr string
	}{
		{name: "image", part: llms.Image("image/png", []byte("x")), want: "image/png"},
		{name: "normalized", part: llms.Image(" Image/JPEG ; q=1", []byte("x")), want: "image/jpeg"},
		{name: "document", part: llms.Document("application/pdf", []byte("x")), want: "application/pdf"},
		{name: "nil media", part: llms.ContentPart{Type: llms.ContentImage}, wantErr: "missing Media"},
		{name: "empty data", part: llms.Document("application/pdf", nil), wantErr: "empty Media.Data"},
		{name: "empty type", part: llms.Image("", []byte("x")), wantErr: "empty Media.MediaType"},
		{name: "unparseable type", part: llms.Image("image/png; =", []byte("x")), wantErr: "invalid Media.MediaType"},
		{name: "no subtype", part: llms.Document("pdf", []byte("x")), wantErr: "invalid Media.MediaType"},
		{name: "image not image", part: llms.Image("application/pdf", []byte("x")), wantErr: "non-image media type"},
		{name: "document is image", part: llms.Document("image/png", []byte("x")), wantErr: "use an image part"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Check(&c.part)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Check = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestCheckRole(t *testing.T) {
	img := llms.Image("image/png", []byte("x"))
	if err := CheckRole(&llms.Message{Role: llms.RoleUser, Content: []llms.ContentPart{img}}); err != nil {
		t.Fatalf("user: %v", err)
	}
	for _, r := range []llms.Role{llms.RoleAssistant, llms.RoleTool} {
		if err := CheckRole(&llms.Message{Role: r, Content: []llms.ContentPart{img}}); err == nil {
			t.Fatalf("%s: media accepted", r)
		}
	}
	if err := CheckRole(&llms.Message{Role: llms.RoleAssistant, Content: []llms.ContentPart{llms.Text("a")}}); err != nil {
		t.Fatalf("assistant text: %v", err)
	}
}

func TestDataURI(t *testing.T) {
	if got := DataURI("image/png", []byte("hi")); got != "data:image/png;base64,aGk=" {
		t.Fatalf("DataURI = %q", got)
	}
}

func encoded(t *testing.T, enc func(*bytes.Buffer, image.Image) error, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := enc(&b, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// webpHeader builds a minimal 30-byte RIFF/WEBP header with the given chunk.
func webpHeader(chunk string, body []byte) []byte {
	b := make([]byte, 30)
	copy(b[0:], "RIFF")
	copy(b[8:], "WEBP")
	copy(b[12:], chunk)
	copy(b[20:], body)
	return b
}

func TestImageSize(t *testing.T) {
	vp8 := make([]byte, 10)
	copy(vp8[3:], []byte{0x9d, 0x01, 0x2a})
	binary.LittleEndian.PutUint16(vp8[6:], 640)
	binary.LittleEndian.PutUint16(vp8[8:], 480)

	vp8l := make([]byte, 5)
	vp8l[0] = 0x2f
	binary.LittleEndian.PutUint32(vp8l[1:], uint32(300-1)|uint32(200-1)<<14)

	vp8x := make([]byte, 10)
	w, h := 5000-1, 3000-1
	vp8x[4], vp8x[5], vp8x[6] = byte(w), byte(w>>8), byte(w>>16)
	vp8x[7], vp8x[8], vp8x[9] = byte(h), byte(h>>8), byte(h>>16)

	cases := []struct {
		name string
		data []byte
		w, h int
		ok   bool
	}{
		{"png", encoded(t, func(b *bytes.Buffer, m image.Image) error { return png.Encode(b, m) }, 123, 45), 123, 45, true},
		{"jpeg", encoded(t, func(b *bytes.Buffer, m image.Image) error { return jpeg.Encode(b, m, nil) }, 64, 32), 64, 32, true},
		{"gif", encoded(t, func(b *bytes.Buffer, m image.Image) error { return gif.Encode(b, m, nil) }, 10, 20), 10, 20, true},
		{"webp lossy", webpHeader("VP8 ", vp8), 640, 480, true},
		{"webp lossless", webpHeader("VP8L", vp8l), 300, 200, true},
		{"webp extended", webpHeader("VP8X", vp8x), 5000, 3000, true},
		{"webp bad start code", webpHeader("VP8 ", make([]byte, 10)), 0, 0, false},
		{"garbage", []byte("not an image at all, just bytes"), 0, 0, false},
		{"empty", nil, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h, ok := ImageSize(c.data)
			if w != c.w || h != c.h || ok != c.ok {
				t.Fatalf("ImageSize = %d, %d, %v; want %d, %d, %v", w, h, ok, c.w, c.h, c.ok)
			}
		})
	}
}

func TestPDFPages(t *testing.T) {
	cases := []struct {
		name string
		data string
		want int
		ok   bool
	}{
		{"page objects", "1 0 obj <</Type /Page>> 2 0 obj <</Type/Page /Parent 3 0 R>> 3 0 obj <</Type /Pages /Kids [1 0 R 2 0 R]>>", 2, true},
		{"count larger", "<</Type /Pages /Count 12 /Kids []>> <</Type /Page>>", 12, true},
		{"page at EOF", "<</Type /Page", 1, true},
		{"pages only, no count", "<</Type /Pages /Kids []>>", 0, false},
		{"compressed streams", "%PDF-1.7 \x78\x9c binary", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := PDFPages([]byte(c.data))
			if got != c.want || ok != c.ok {
				t.Fatalf("PDFPages = %d, %v; want %d, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestCheckToolResult(t *testing.T) {
	ok := []llms.ToolResult{
		{Content: "plain"},
		{},
		{Parts: []llms.ContentPart{llms.Text("a"), llms.Image("image/png", []byte("x")), llms.Document("application/pdf", []byte("x"))}},
	}
	for i := range ok {
		if err := CheckToolResult(&ok[i]); err != nil {
			t.Errorf("result %d rejected: %v", i, err)
		}
	}
	bad := map[string]llms.ToolResult{
		"both":       {Content: "x", Parts: []llms.ContentPart{llms.Text("y")}},
		"empty text": {Parts: []llms.ContentPart{llms.Text("")}},
		"bad media":  {Parts: []llms.ContentPart{llms.Document("image/png", []byte("x"))}},
		"nested":     {Parts: []llms.ContentPart{{Type: llms.ContentToolResult, ToolResult: &llms.ToolResult{}}}},
		"unknown":    {Parts: []llms.ContentPart{{Type: "audio"}}},
	}
	for name, r := range bad {
		if err := CheckToolResult(&r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckEmbeddingInput(t *testing.T) {
	ok := []llms.EmbeddingInput{
		{Text: "plain"},
		{},
		{Title: "T", Parts: []llms.ContentPart{llms.Text("a"), llms.Image("image/png", []byte("x")), llms.Document("application/pdf", []byte("x"))}},
	}
	for i := range ok {
		if err := CheckEmbeddingInput(&ok[i]); err != nil {
			t.Errorf("input %d rejected: %v", i, err)
		}
	}
	bad := map[string]llms.EmbeddingInput{
		"both":       {Text: "x", Parts: []llms.ContentPart{llms.Text("y")}},
		"empty text": {Parts: []llms.ContentPart{llms.Text("")}},
		"bad media":  {Parts: []llms.ContentPart{llms.Image("application/pdf", []byte("x"))}},
		"no bytes":   {Parts: []llms.ContentPart{llms.Image("image/png", nil)}},
		"tool call":  {Parts: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "1", Name: "f"}}}},
		"unknown":    {Parts: []llms.ContentPart{{Type: "audio"}}},
	}
	for name, in := range bad {
		if err := CheckEmbeddingInput(&in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestJoinTextAndHasMedia(t *testing.T) {
	parts := []llms.ContentPart{llms.Text("a"), llms.Image("image/png", []byte("x")), llms.Text("b")}
	if got := JoinText(parts); got != "a\n\nb" {
		t.Fatalf("JoinText = %q", got)
	}
	if !HasMedia(parts) || HasMedia(parts[:1]) {
		t.Fatal("HasMedia wrong")
	}
}

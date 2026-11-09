package middleware

import (
	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/media"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/satint"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
)

// TokenEstimator estimates the rate-limit-relevant size of a request before it
// is sent. Estimates should overestimate: for a limiter, overestimation is the
// safe error.
type TokenEstimator interface {
	EstimateChat(req llms.ChatRequest) ratelimit.UsageUnits
	EstimateEmbeddings(req llms.EmbeddingRequest) ratelimit.UsageUnits
}

// estimator tuning. These are deliberately conservative (bias high).
const (
	// charsPerToken is intentionally low (~3) so character-count-based
	// estimates run higher than the typical ~4 chars/token, overestimating.
	charsPerToken = 3
	// perPartOverhead pads each content part for role/formatting tokens.
	perPartOverhead = 8
	// defaultOutputMax is assumed output budget when a request sets no
	// MaxTokens, so the reservation still accounts for generated tokens.
	defaultOutputMax = 1024
)

// Media tuning (ADR-0040). The figures are provider-documented, provider-
// neutral and model-neutral: each covers the largest documented cost among
// Anthropic, OpenAI and Gemini rather than predicting one model's.
const (
	// pixelsPerImageToken is Anthropic's formula (tokens = pixels/750),
	// applied without its resize cap so it also covers models that don't
	// downscale.
	pixelsPerImageToken = 750
	// patchSize and patchTokenNum/patchTokenDen are OpenAI's patch-based
	// formula: ceil(w/32)*ceil(h/32) patches at 1.2 tokens each.
	patchSize     = 32
	patchTokenNum = 6
	patchTokenDen = 5
	// minImageTokens is Gemini 3's default per-image cost, the largest
	// documented flat charge for a small image.
	minImageTokens = 1120
	// unknownImageTokens is charged when an image's dimensions can't be
	// read, and caps every image estimate: OpenAI's largest accepted image
	// (30,000 patches at 1.2), the largest documented per-image cost
	// (Anthropic and Gemini downscale large images).
	unknownImageTokens = 36000
	// pdfPageTokens is a typical-high per-page figure (Anthropic documents
	// 1,500-3,000 text tokens per page plus the page as an image). It is not
	// a strict bound: an app that knows its page counts or model can supply
	// its own TokenEstimator.
	pdfPageTokens = 4000
	// documentOverheadTokens pads every document for the provider's
	// framing around it (Anthropic billed 72 tokens for a 50-byte text
	// document with a short question, against 52 counted as text).
	documentOverheadTokens = 100
)

// DefaultEstimator is an approximate, provider-neutral TokenEstimator based on
// UTF-8 byte counts. It does not tokenize; it trades accuracy for zero
// dependencies and a safe upward bias. Image and document parts are counted
// from provider-documented figures (ADR-0040): an image from its pixel
// dimensions, a PDF at a typical-high 4,000 tokens per page. Provider-specific
// estimators may be added later.
type DefaultEstimator struct{}

func tokensForText(s string) int {
	if s == "" {
		return 0
	}
	return ceilDiv(len(s), charsPerToken)
}

func tokensForParts(parts []llms.ContentPart) int {
	total := 0
	for i := range parts {
		total = sum(total, tokensForPart(&parts[i]))
	}
	return total
}

// tokensForPart estimates one content part.
func tokensForPart(p *llms.ContentPart) int {
	n := sum(perPartOverhead, tokensForText(p.Text))
	if p.ToolCall != nil {
		n = sum(n, tokensForText(p.ToolCall.Name), tokensForText(string(p.ToolCall.Parameters)),
			tokensForText(string(p.ToolCall.ProviderSignature)))
	}
	if p.ToolResult != nil {
		n = sum(n, tokensForText(p.ToolResult.Content), tokensForResultParts(p.ToolResult.Parts))
	}
	if p.Reasoning != nil {
		// Opaque provider data is sent back verbatim, so count all of
		// it (it already holds the readable reasoning, so Text isn't
		// added again). For a signed or encrypted payload this runs
		// high, which is the safe direction for a reservation.
		n = sum(n, tokensForText(string(p.Reasoning.ProviderData)))
	}
	if p.Media != nil && media.IsMedia(p.Type) {
		n = sum(n, tokensForMedia(p.Type, p.Media))
	}
	return n
}

// tokensForResultParts estimates a tool result's Parts (ADR-0041): text and
// media as in a message, each with the per-part overhead. It does not
// recurse, since result parts never nest (and a malformed one must not loop).
func tokensForResultParts(parts []llms.ContentPart) int {
	total := 0
	for i := range parts {
		p := &parts[i]
		n := sum(perPartOverhead, tokensForText(p.Text))
		if p.Media != nil && media.IsMedia(p.Type) {
			n = sum(n, tokensForMedia(p.Type, p.Media))
		}
		total = sum(total, n)
	}
	return total
}

// sum adds token counts, saturating at math.MaxInt. Every estimator sum goes
// through it: a media estimate can be very large, and a wrapped sum would
// turn an oversized request into a small (or zero) reservation.
func sum(counts ...int) int {
	total := 0
	for _, n := range counts {
		total = satint.Add(total, n)
	}
	return total
}

// ceilDiv returns n/d rounded up for n >= 0, d > 0, without the n+d-1 sum
// that would wrap when n is near math.MaxInt.
func ceilDiv(n, d int) int {
	q := n / d
	if n%d != 0 {
		q++
	}
	return q
}

// addTools adds the tool definitions' tokens to input.
func addTools(input int, tools []llms.ToolDefinition) int {
	for i := range tools {
		input = sum(input, tokensForText(tools[i].Name), tokensForText(tools[i].Description),
			tokensForText(string(tools[i].InputSchema)))
	}
	return input
}

// tokensForMedia estimates an image or document part (ADR-0040). An image is
// sized from its header: the largest of the pixel formula, the patch formula
// and the per-image floor, capped at unknownImageTokens, which is also the
// charge when the header doesn't parse. A PDF costs pdfPageTokens per page, falling back to its byte count
// as text when the page count isn't visible; a text document counts as text;
// any other document counts its bytes as text. Every document adds
// documentOverheadTokens for the provider's framing.
func tokensForMedia(t llms.ContentPartType, m *llms.Media) int {
	base, _ := media.Normalize(m.MediaType)
	if t == llms.ContentImage {
		w, h, ok := media.ImageSize(m.Data)
		if !ok {
			return unknownImageTokens
		}
		pixels := satint.Mul(w, h)
		byPixels := ceilDiv(pixels, pixelsPerImageToken)
		patches := satint.Mul(ceilDiv(w, patchSize), ceilDiv(h, patchSize))
		byPatches := satint.Mul(patches, patchTokenNum) / patchTokenDen
		return min(max(byPixels, byPatches, minImageTokens), unknownImageTokens)
	}
	if base == "application/pdf" {
		if pages, ok := media.PDFPages(m.Data); ok {
			return satint.Add(documentOverheadTokens, satint.Mul(pages, pdfPageTokens))
		}
	}
	return sum(documentOverheadTokens, ceilDiv(len(m.Data), charsPerToken))
}

// EstimateChat estimates input tokens from system + messages + tool schemas,
// and output tokens from MaxTokens (or a default), biased high.
func (DefaultEstimator) EstimateChat(req llms.ChatRequest) ratelimit.UsageUnits {
	input := tokensForParts(req.System)
	for i := range req.Messages {
		input = sum(input, tokensForParts(req.Messages[i].Content))
	}
	input = addTools(input, req.Tools)
	outMax := req.MaxTokens
	if outMax <= 0 {
		outMax = defaultOutputMax
	}
	return ratelimit.UsageUnits{
		InputTokens:     input,
		OutputTokensMax: outMax,
		TotalTokensMax:  satint.Add(input, outMax), // MaxTokens may be MaxInt for "no cap"
		Requests:        1,
	}
}

// EstimateEmbeddings estimates input tokens from the concatenated inputs.
// Embeddings produce no output tokens.
func (DefaultEstimator) EstimateEmbeddings(req llms.EmbeddingRequest) ratelimit.UsageUnits {
	input := 0
	for i := range req.Inputs {
		input = sum(input, tokensForText(req.Inputs[i].Text))
	}
	return ratelimit.UsageUnits{
		InputTokens:    input,
		TotalTokensMax: input,
		Requests:       1,
	}
}

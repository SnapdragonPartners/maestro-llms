package google

import (
	"context"
	"fmt"
	"math"
	"strings"

	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/media"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/satint"
)

// EmbeddingClient is an llms.EmbeddingClient backed by genai (Gemini /
// Vertex). Its size is dominated by the embedded SDK client.
//
//nolint:govet // fieldalignment: size dominated by the embedded SDK client
type EmbeddingClient struct {
	api           *genai.Client
	model         string
	dims          int
	autoTruncate  bool
	maxInputBytes int
	vertex        bool
}

// defaultBatchLimit bounds inputs for models without a known single-input
// constraint (Vertex text-embedding models cap at 250 per call).
const defaultBatchLimit = 250

// baseModel strips genai/Vertex path and "models/" prefixes so the
// single-input rule still matches "models/gemini-embedding-001" and
// "projects/.../publishers/google/models/gemini-embedding-001".
func baseModel(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	return model
}

// embedContentRoute reports whether genai sends model to Vertex's
// :embedContent API rather than predict. It mirrors genai v1.77's
// tIsVertexEmbedContentModel on the raw model string, so a resource path
// whose project or location contains "gemini" or "maas" matches as genai
// does (ADR-0045 item 9). TestEmbedContentRouteMatchesSDK pins the SDK's
// behavior. On :embedContent genai sends one content per call with all its
// parts; on predict it sends only each content's first text part.
func embedContentRoute(model string) bool {
	return (strings.Contains(model, "gemini") && model != "gemini-embedding-001") ||
		strings.Contains(model, "maas")
}

// batchLimit is the max inputs per call. Single-input models return 1: a
// multi-input request is rejected up front with a typed error; the client
// never fans out or chunks (the application owns batching; ADR-0009).
// gemini-embedding-001 is single-input on both backends (by base name, so a
// full path matches too), and on Vertex so is every model on the
// :embedContent route, where genai takes one content per call (ADR-0045).
func batchLimit(model string, vertex bool) int {
	if baseModel(model) == "gemini-embedding-001" || (vertex && embedContentRoute(model)) {
		return 1
	}
	return defaultBatchLimit
}

// embedConfig validates the options for NewEmbeddings and builds the genai
// client config: the shared backend selection (clientConfig) plus the
// fail-closed truncation contract.
func embedConfig(s *settings) (*genai.ClientConfig, error) {
	if s.dims < 0 || s.dims > math.MaxInt32 {
		return nil, configErr("WithDimensions must be >= 0 and fit int32 (0 = model default)")
	}
	cc, err := s.clientConfig()
	if err != nil {
		return nil, err
	}

	// Fail-closed truncation contract (ADR-0009 refinement): genai cannot
	// send autoTruncate:false, so AutoTruncate=true is Vertex-only, and
	// AutoTruncate=false needs a client-side guard (MaxInputBytes>0) — else
	// the API would look safe while Vertex silently truncates.
	if s.autoTruncate {
		if cc.Backend == genai.BackendGeminiAPI {
			return nil, configErr("WithAutoTruncate is Vertex-only; the Gemini API backend does not support it")
		}
	} else if s.maxInputBytes <= 0 {
		return nil, configErr("set WithMaxInputBytes (no-silent-truncation guard) or WithAutoTruncate(true): " +
			"genai cannot send autoTruncate:false, so without a client-side guard Vertex would silently truncate")
	}
	return cc, nil
}

// NewEmbeddings builds a Gemini/Vertex embedding client. The backend is
// selected by the options: WithAPIKey -> the direct Gemini API;
// WithVertex + WithCredentials -> Vertex AI (Morris's supported path).
// WithModel is required, and so is WithMaxInputBytes unless
// WithAutoTruncate(true) on Vertex. WithBaseURL / WithHTTPClient are the
// Private Service Connect injection points, the application's / OpenTofu's
// concern (ADR-0009). It returns a *llms.ProviderError of kind config when
// required values are missing or inconsistent.
func NewEmbeddings(opts ...Option) (*EmbeddingClient, error) {
	s := applyOptions(opts)
	cc, err := embedConfig(&s)
	if err != nil {
		return nil, err
	}

	api, err := genai.NewClient(context.Background(), cc)
	if err != nil {
		return nil, &llms.ProviderError{
			Provider: providerName, Kind: llms.ErrorKindConfig,
			Message: "genai client init failed", Cause: err,
		}
	}
	return &EmbeddingClient{
		api: api, model: s.model, dims: s.dims,
		autoTruncate: s.autoTruncate, maxInputBytes: s.maxInputBytes,
		vertex: cc.Backend == genai.BackendVertexAI,
	}, nil
}

// Model returns the model this client targets.
func (c *EmbeddingClient) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// DefaultDimensions returns the configured default output dimensionality
// (0 = the model default); EmbeddingRequest.Dimensions overrides it.
func (c *EmbeddingClient) DefaultDimensions() int { return c.dims }

// taskType maps the neutral EmbeddingTask to Gemini's task-type string. The
// zero value yields "" (the SDK omits taskType).
func taskType(t llms.EmbeddingTask) string {
	switch t {
	case llms.EmbeddingTaskRetrievalDocument:
		return "RETRIEVAL_DOCUMENT"
	case llms.EmbeddingTaskRetrievalQuery:
		return "RETRIEVAL_QUERY"
	case llms.EmbeddingTaskSemanticSimilarity:
		return "SEMANTIC_SIMILARITY"
	case llms.EmbeddingTaskClassification:
		return "CLASSIFICATION"
	case llms.EmbeddingTaskClustering:
		return "CLUSTERING"
	case llms.EmbeddingTaskQuestionAnswering:
		return "QUESTION_ANSWERING"
	case llms.EmbeddingTaskFactVerification:
		return "FACT_VERIFICATION"
	case llms.EmbeddingTaskCodeRetrievalQuery:
		return "CODE_RETRIEVAL_QUERY"
	case llms.EmbeddingTaskUnspecified:
		return ""
	default:
		return ""
	}
}

// Embed embeds req.Inputs, preserving input order/IDs. It rejects oversized
// input up front unless AutoTruncate was set (ADR-0009), rejects a
// multi-input request for single-input models, and maps each input's Parts
// for the API route genai will use (ADR-0045).
func (c *EmbeddingClient) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	if len(req.Inputs) == 0 {
		return llms.EmbeddingResponse{}, badRequest(c.model, "no inputs")
	}
	if req.Dimensions < 0 || req.Dimensions > math.MaxInt32 {
		return llms.EmbeddingResponse{}, badRequest(c.model, "Dimensions must be >= 0 and fit int32")
	}
	if limit := batchLimit(c.model, c.vertex); len(req.Inputs) > limit {
		return llms.EmbeddingResponse{}, badRequest(c.model,
			"too many inputs for this model: the application owns chunking/batching")
	}

	contents := make([]*genai.Content, len(req.Inputs))
	for i := range req.Inputs {
		content, textBytes, perr := c.content(i, &req.Inputs[i])
		if perr != nil {
			return llms.EmbeddingResponse{}, perr
		}
		// No-silent-truncation guard: when not opting into provider
		// truncation, reject oversized text here (genai cannot send
		// autoTruncate:false, so this client-side byte check is the
		// enforcement; ADR-0009). It measures the text as sent, so joined
		// separators count, and media bytes don't (ADR-0045 item 8). The
		// application owns chunking; it sees a typed error, not a silent cut.
		if !c.autoTruncate && textBytes > c.maxInputBytes {
			return llms.EmbeddingResponse{}, badRequest(c.model, fmt.Sprintf(
				"input %d is %d bytes, exceeds MaxInputBytes %d (set AutoTruncate or chunk smaller)",
				i, textBytes, c.maxInputBytes))
		}
		contents[i] = content
	}

	ecfg := &genai.EmbedContentConfig{
		TaskType:     taskType(req.Task),
		AutoTruncate: c.autoTruncate,
	}
	if d := outputDims(c.dims, req.Dimensions); d != nil {
		ecfg.OutputDimensionality = d
	}
	// Title is a single per-call field in genai; only meaningful with a
	// retrieval-document task. Honor it for the single-input case (the
	// gemini-embedding-001 path); per-input titles for batch models are not
	// representable and are left unset.
	if req.Task == llms.EmbeddingTaskRetrievalDocument && len(req.Inputs) == 1 {
		ecfg.Title = req.Inputs[0].Title
	}

	resp, err := c.api.Models.EmbedContent(ctx, c.model, contents, ecfg)
	if err != nil {
		return llms.EmbeddingResponse{}, classifyError(c.model, err)
	}
	if len(resp.Embeddings) != len(req.Inputs) {
		return llms.EmbeddingResponse{}, badRequest(c.model,
			"provider returned a different number of vectors than inputs")
	}

	vectors := make([]llms.EmbeddingVector, len(req.Inputs))
	for i := range req.Inputs {
		vectors[i] = llms.EmbeddingVector{ID: req.Inputs[i].ID, Values: resp.Embeddings[i].Values}
	}
	return llms.EmbeddingResponse{Vectors: vectors, Usage: embedUsage(resp.Embeddings), Raw: resp}, nil
}

// content translates one input into the genai content to send and the number
// of text bytes in it, as sent (ADR-0045 items 7 and 8). Text maps to one
// text part. Parts map 1:1 on the Gemini API and on Vertex's :embedContent
// route; on Vertex's predict route genai would keep only the first part's
// text, so text parts are joined into one and media is a bad_request.
func (c *EmbeddingClient) content(i int, in *llms.EmbeddingInput) (*genai.Content, int, *llms.ProviderError) {
	if err := media.CheckEmbeddingInput(in); err != nil {
		return nil, 0, badRequest(c.model, fmt.Sprintf("embedding input %d: %v", i, err))
	}
	if len(in.Parts) == 0 {
		return genai.NewContentFromText(in.Text, genai.RoleUser), len(in.Text), nil
	}
	if c.vertex && !embedContentRoute(c.model) {
		if media.HasMedia(in.Parts) {
			return nil, 0, badRequest(c.model, fmt.Sprintf(
				"embedding input %d: this model is served by Vertex's predict API, which takes text only; "+
					"image and document parts need a model on the embedContent API (a Gemini embedding model other than gemini-embedding-001)", i))
		}
		text := media.JoinText(in.Parts)
		return genai.NewContentFromText(text, genai.RoleUser), len(text), nil
	}
	parts := make([]*genai.Part, len(in.Parts))
	textBytes := 0
	for j := range in.Parts {
		p := &in.Parts[j]
		if p.Type == llms.ContentText {
			parts[j] = genai.NewPartFromText(p.Text)
			textBytes = satint.Add(textBytes, len(p.Text))
			continue
		}
		base, err := media.Check(p) // already checked; this returns the normalized type
		if err != nil {
			return nil, 0, badRequest(c.model, fmt.Sprintf("embedding input %d part %d: %v", i, j, err))
		}
		parts[j] = &genai.Part{InlineData: &genai.Blob{MIMEType: base, Data: p.Media.Data}}
	}
	return genai.NewContentFromParts(parts, genai.RoleUser), textBytes, nil
}

// embedUsage sums the per-embedding token counts Vertex reports (genai puts
// them in Statistics on both the predict and :embedContent routes; ADR-0045
// item 11), each rounded up. If any embedding lacks a positive count (the
// Gemini API reports none, and genai sets empty statistics on :embedContent
// when usageMetadata is absent), Usage stays zero rather than undercounting.
func embedUsage(embs []*genai.ContentEmbedding) llms.Usage {
	total := 0
	for _, e := range embs {
		if e == nil || e.Statistics == nil || !(e.Statistics.TokenCount > 0) {
			return llms.Usage{}
		}
		n := math.Ceil(float64(e.Statistics.TokenCount))
		if n >= math.MaxInt32 {
			n = math.MaxInt32
		}
		total = satint.Add(total, int(n))
	}
	return llms.Usage{EmbeddingTokens: total, TotalTokens: total}
}

// outputDims returns the OutputDimensionality pointer: a per-request override
// wins over the client default; nil means "use the model default".
func outputDims(def, override int) *int32 {
	d := def
	if override > 0 {
		d = override
	}
	if d <= 0 {
		return nil
	}
	v := int32(d) //nolint:gosec // embedding dims are small positive ints
	return &v
}

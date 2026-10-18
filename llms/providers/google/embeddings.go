package google

import (
	"context"
	"fmt"
	"math"
	"strings"

	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms"
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
}

// defaultBatchLimit bounds inputs for models without a known single-input
// constraint (Vertex text-embedding models cap at 250 per call).
const defaultBatchLimit = 250

// batchLimit is the max inputs per call. Single-input models (e.g.
// gemini-embedding-001) return 1: a multi-input request is rejected up front
// with a typed error — the client never fans out or chunks (the application
// owns batching; ADR-0009).
// baseModel strips genai/Vertex path and "models/" prefixes so the
// single-input rule still matches "models/gemini-embedding-001" and
// "projects/.../publishers/google/models/gemini-embedding-001".
func baseModel(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	return model
}

func batchLimit(model string) int {
	switch baseModel(model) {
	case "gemini-embedding-001":
		return 1
	default:
		return defaultBatchLimit
	}
}

// embedConfig validates the options for NewEmbeddings and builds the genai
// client config: backend selection (fail closed on ambiguity), the
// fail-closed truncation contract, and endpoint/transport injection.
func embedConfig(s *settings) (*genai.ClientConfig, error) {
	if err := s.commonErr(); err != nil {
		return nil, err
	}
	vertex := len(s.vertexOpts) > 0
	switch {
	case s.dims < 0 || s.dims > math.MaxInt32:
		return nil, configErr("WithDimensions must be >= 0 and fit int32 (0 = model default)")
	case s.apiKey != "" && vertex:
		// genai treats API key and Vertex (project/location/credentials) as
		// mutually exclusive. Fail closed rather than silently pick the
		// public API-key backend and bypass Vertex/PSC.
		return nil, configErr("ambiguous backend: set WithAPIKey (Gemini API) OR WithVertex+WithCredentials (Vertex), not both")
	}

	cc, err := embedBackend(s, vertex)
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

	if s.baseURL != "" {
		cc.HTTPOptions.BaseURL = s.baseURL
	}
	if s.httpClient != nil {
		cc.HTTPClient = s.httpClient
	}
	return cc, nil
}

// embedBackend selects the genai backend: the Gemini API by key, or Vertex
// with app-supplied credentials.
func embedBackend(s *settings, vertex bool) (*genai.ClientConfig, error) {
	cc := &genai.ClientConfig{}
	switch {
	case s.apiKey != "":
		cc.Backend = genai.BackendGeminiAPI
		cc.APIKey = s.apiKey
	case vertex:
		switch {
		case s.project == "" || s.location == "":
			return nil, configErr("Vertex backend requires WithVertex with a non-empty project and location")
		case s.creds == nil:
			return nil, configErr("Vertex backend requires WithCredentials (this package does no ADC discovery)")
		case s.creds.TokenProvider == nil:
			// Credentials that cannot mint tokens fail every call; reject
			// them up front, as anthropicvertex does.
			return nil, configErr("Google credentials have no TokenProvider (refusing to fall back to ADC)")
		}
		cc.Backend = genai.BackendVertexAI
		cc.Project, cc.Location, cc.Credentials = s.project, s.location, s.creds
	default:
		return nil, configErr("missing credentials: set WithAPIKey (Gemini API) or WithVertex+WithCredentials (Vertex)")
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
// input up front unless AutoTruncate was set (ADR-0009), and rejects a
// multi-input request for single-input models.
func (c *EmbeddingClient) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	if len(req.Inputs) == 0 {
		return llms.EmbeddingResponse{}, badRequest(c.model, "no inputs")
	}
	if req.Dimensions < 0 || req.Dimensions > math.MaxInt32 {
		return llms.EmbeddingResponse{}, badRequest(c.model, "Dimensions must be >= 0 and fit int32")
	}
	if limit := batchLimit(c.model); len(req.Inputs) > limit {
		return llms.EmbeddingResponse{}, badRequest(c.model,
			"too many inputs for this model: the application owns chunking/batching")
	}
	// No-silent-truncation guard: when not opting into provider truncation,
	// reject oversized input here (genai cannot send autoTruncate:false, so
	// this client-side byte check is the enforcement — ADR-0009). The
	// application owns chunking; it sees a typed error, not a silent cut.
	if !c.autoTruncate {
		for i := range req.Inputs {
			if n := len(req.Inputs[i].Text); n > c.maxInputBytes {
				return llms.EmbeddingResponse{}, badRequest(c.model, fmt.Sprintf(
					"input %d is %d bytes, exceeds MaxInputBytes %d (set AutoTruncate or chunk smaller)",
					i, n, c.maxInputBytes))
			}
		}
	}

	contents := make([]*genai.Content, len(req.Inputs))
	for i := range req.Inputs {
		contents[i] = genai.NewContentFromText(req.Inputs[i].Text, genai.RoleUser)
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
	// genai reports BillableCharacterCount, not tokens; per the spec, leave
	// Usage zero when the provider does not report tokens.
	return llms.EmbeddingResponse{Vectors: vectors, Raw: resp}, nil
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

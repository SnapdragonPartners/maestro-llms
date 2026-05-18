package google

import (
	"context"
	"fmt"
	"net/http"

	"cloud.google.com/go/auth"
	"google.golang.org/genai"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// EmbeddingConfig configures an EmbeddingClient. Backend is selected by which
// fields are set: APIKey -> the direct Gemini API; otherwise Project (+
// Location + Credentials) -> Vertex AI. Morris's supported path is Vertex.
// The zero value is invalid (Model is required).
//
//nolint:govet // fieldalignment: caller-facing config; logical field grouping over 8 bytes.
type EmbeddingConfig struct {
	// Model is required, e.g. "gemini-embedding-001".
	Model string

	// APIKey selects the direct Gemini API backend.
	APIKey string

	// Project / Location / Credentials select the Vertex AI backend.
	// Credentials is app-supplied; this package does NO ADC discovery.
	Project     string
	Location    string
	Credentials *auth.Credentials

	// Endpoint overrides the base URL (Private Service Connect). The
	// HTTPClient is the network transport (PSC dialer / custom CA). Both are
	// the application's / OpenTofu's concern (ADR-0009).
	Endpoint   string
	HTTPClient *http.Client

	// Dimensions is the default output dimensionality (0 = model default);
	// EmbeddingRequest.Dimensions overrides it per call.
	Dimensions int

	// MaxInputBytes is the no-silent-truncation guard: when AutoTruncate is
	// false, an input whose Text exceeds this many UTF-8 bytes is rejected
	// with a typed bad_request before the provider call. It is a literal
	// byte count (not tokens — conservative, mechanical, auditable). The
	// application sets it from its model/chunking policy. Required when
	// AutoTruncate is false.
	MaxInputBytes int

	// AutoTruncate controls oversized-input handling. genai's
	// EmbedContentConfig.AutoTruncate is `bool,omitempty`, so a false value
	// is UNREPRESENTABLE on the wire — genai cannot send autoTruncate:false,
	// and Vertex then applies its server default (silently truncate). So:
	//
	//   - AutoTruncate=true: opt into provider/server truncation. Vertex-only
	//     (the Gemini API rejects the parameter); construction fails on the
	//     Gemini-API backend.
	//   - AutoTruncate=false (default) + MaxInputBytes>0: this package
	//     enforces no-silent-truncation itself by rejecting oversized input
	//     up front (see MaxInputBytes).
	//   - AutoTruncate=false + MaxInputBytes<=0: NO guarantee is possible
	//     through genai, so NewEmbeddings FAILS rather than look safe while
	//     Vertex silently truncates (ADR-0009 refinement).
	AutoTruncate bool
}

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
func batchLimit(model string) int {
	switch model {
	case "gemini-embedding-001":
		return 1
	default:
		return defaultBatchLimit
	}
}

// NewEmbeddings builds a Gemini/Vertex embedding client. It returns a
// *llms.ProviderError of kind config when required values are missing.
func NewEmbeddings(cfg EmbeddingConfig) (*EmbeddingClient, error) {
	if cfg.Model == "" {
		return nil, configErr("missing model")
	}

	cc := &genai.ClientConfig{}
	switch {
	case cfg.APIKey != "":
		cc.Backend = genai.BackendGeminiAPI
		cc.APIKey = cfg.APIKey
	case cfg.Project != "":
		cc.Backend = genai.BackendVertexAI
		cc.Project = cfg.Project
		if cfg.Location == "" {
			return nil, configErr("Vertex backend requires a location")
		}
		cc.Location = cfg.Location
		if cfg.Credentials == nil {
			return nil, configErr("Vertex backend requires Credentials (this package does no ADC discovery)")
		}
		cc.Credentials = cfg.Credentials
	default:
		return nil, configErr("missing credentials: set APIKey (Gemini API) or Project+Location+Credentials (Vertex)")
	}

	// Fail-closed truncation contract (ADR-0009 refinement). genai cannot
	// send autoTruncate:false, so:
	//   - AutoTruncate=true is Vertex-only (Gemini API rejects it).
	//   - AutoTruncate=false needs a client-side guard (MaxInputBytes>0),
	//     else the API would look safe while Vertex silently truncates.
	if cfg.AutoTruncate {
		if cc.Backend == genai.BackendGeminiAPI {
			return nil, configErr("AutoTruncate is Vertex-only; the Gemini API backend does not support it")
		}
	} else if cfg.MaxInputBytes <= 0 {
		return nil, configErr("set MaxInputBytes (no-silent-truncation guard) or AutoTruncate=true: " +
			"genai cannot send autoTruncate:false, so without a client-side guard Vertex would silently truncate")
	}

	if cfg.Endpoint != "" {
		cc.HTTPOptions.BaseURL = cfg.Endpoint
	}
	if cfg.HTTPClient != nil {
		cc.HTTPClient = cfg.HTTPClient
	}

	api, err := genai.NewClient(context.Background(), cc)
	if err != nil {
		return nil, &llms.ProviderError{
			Provider: providerName, Kind: llms.ErrorKindConfig,
			Message: "genai client init failed", Cause: err,
		}
	}
	return &EmbeddingClient{
		api: api, model: cfg.Model, dims: cfg.Dimensions,
		autoTruncate: cfg.AutoTruncate, maxInputBytes: cfg.MaxInputBytes,
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

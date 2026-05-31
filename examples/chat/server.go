package main

// server.go contains the HTTP handlers + the per-request chat wiring.
// The wiring is the part developers should read most carefully — it
// shows where RecommendedChat plugs in, why EstimateTextTokens is used
// for the UI's "approx tokens" indicator, and how a fresh ChatClient is
// constructed per request (so the demo never accidentally reuses a
// client across different selected models).

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/vllm"
)

//go:embed ui/index.html ui/styles.css ui/app.js
var uiFS embed.FS

// newServer wires routes: the single-page UI, a JSON endpoint listing
// the dropdown options, and the chat POST endpoint.
func newServer(options []ModelOption, logger *log.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", servePage)
	mux.Handle("/static/", http.StripPrefix("/static/", serveStatic()))
	mux.HandleFunc("/api/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, options)
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		handleChat(w, r, options, logger)
	})
	return mux
}

// servePage serves the index.html from the embedded UI bundle.
func servePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// serveStatic exposes the styles + script bundle under /static/.
func serveStatic() http.Handler {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		// embed.FS only fails like this for a path that doesn't exist
		// — a compile-time-ish assertion that we wired the directory
		// correctly.
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}

// chatRequest is the body the UI POSTs to /api/chat. ModelID is the
// dropdown's value (e.g. "anthropic/claude-opus-4-7-20251015"); History
// is the running conversation including the latest user message.
type chatRequest struct {
	ModelID string `json:"modelId"`
	History []apiMessage `json:"history"`
}

type apiMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type chatResponse struct {
	Text                 string `json:"text"`
	Model                string `json:"model"`                // raw model ID from the response; shown in the UI footer
	StopReason           string `json:"stopReason"`           // RAW provider finish reason (cross-provider variance is intentional; UI normalizes for display)
	InputTokens          int    `json:"inputTokens"`
	OutputTokens         int    `json:"outputTokens"`         // visible only (ADR-0016 normalization)
	ReasoningTokens      int    `json:"reasoningTokens"`      // ADR-0016 — non-visible thinking budget, zero for non-reasoning models
	BillableOutputTokens int    `json:"billableOutputTokens"` // ADR-0016 — what the provider bills as "output"; surfaced for DevTools / cost math
	TotalTokens          int    `json:"totalTokens"`
	LatencyMS            int64  `json:"latencyMs"`
	Error                string `json:"error,omitempty"`
}

func handleChat(w http.ResponseWriter, r *http.Request, options []ModelOption, logger *log.Logger) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, chatResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	if len(req.History) == 0 {
		writeJSON(w, http.StatusBadRequest, chatResponse{Error: "history is empty"})
		return
	}
	opt, ok := findOption(options, req.ModelID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, chatResponse{Error: "unknown model: " + req.ModelID})
		return
	}

	// Build a fresh ChatClient for the selected provider. We deliberately
	// do not cache clients across requests in this demo: a single user is
	// going to flip between providers/models freely and the construction
	// cost is negligible (it's just option-record assembly; the SDK
	// clients are concurrency-safe so caching would be a pure
	// optimization, not a correctness need).
	base, err := buildClient(opt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, chatResponse{Error: err.Error()})
		return
	}

	// Wrap in the recommended middleware. This is the pedagogical heart
	// of the demo: any production service would do roughly the same
	// thing. The per-attempt timeout in particular is worth showing —
	// without it a stuck provider hangs the whole request indefinitely.
	client := middleware.RecommendedChat(base, middleware.RecommendedConfig{
		Timeout: 30 * time.Second,
		// In a real service you'd also pass Observer for metrics and
		// Limiter for rate-limit reservations. We skip both for the
		// demo to keep the wiring tight.
	})

	// Translate the UI's message list into the toolkit's neutral shape.
	msgs := make([]llms.Message, 0, len(req.History))
	for _, m := range req.History {
		switch m.Role {
		case "user":
			msgs = append(msgs, llms.UserText(m.Text))
		case "assistant":
			msgs = append(msgs, llms.AssistantText(m.Text))
		default:
			writeJSON(w, http.StatusBadRequest, chatResponse{Error: "unknown role: " + m.Role})
			return
		}
	}

	chatReq := llms.ChatRequest{
		Purpose:   llms.PurposeChat,
		Messages:  msgs,
		MaxTokens: 1024,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := client.Complete(ctx, chatReq)
	latency := time.Since(start)
	if err != nil {
		logger.Printf("provider %s: %v", opt.ProviderName, err)
		writeJSON(w, http.StatusBadGateway, chatResponse{Error: friendlyErr(err)})
		return
	}

	writeJSON(w, http.StatusOK, chatResponse{
		Text:                 resp.Text,
		Model:                opt.Model,
		StopReason:           string(resp.StopReason),
		InputTokens:          resp.Usage.InputTokens,
		OutputTokens:         resp.Usage.OutputTokens,
		ReasoningTokens:      resp.Usage.ReasoningTokens,
		BillableOutputTokens: resp.Usage.BillableOutputTokens,
		TotalTokens:          resp.Usage.TotalTokens,
		LatencyMS:            latency.Milliseconds(),
	})
}

// buildClient constructs the concrete ChatClient for the selected
// provider+model. Each branch mirrors what the dropdown population logic
// does in providers.go; the demo trades a small amount of duplication
// for keeping each provider's wiring straightforwardly readable.
func buildClient(opt ModelOption) (llms.ChatClient, error) {
	switch opt.ProviderName {
	case "anthropic":
		key := envFirst("ANTHROPIC_API_KEY", "MAESTRO_ANTHROPIC_API_KEY")
		return anthropic.New(anthropic.WithAPIKey(key), anthropic.WithModel(opt.Model))
	case "openai":
		return openai.NewChat(openai.WithAPIKey(envFirst("OPENAI_API_KEY")), openai.WithModel(opt.Model))
	case "google":
		key := envFirst("GEMINI_API_KEY", "GOOGLE_GENAI_API_KEY", "GOOGLE_API_KEY")
		return google.New(google.WithAPIKey(key), google.WithModel(opt.Model))
	case "ollama":
		host := envFirst("OLLAMA_HOST")
		if host == "" {
			host = "http://localhost:11434"
		}
		return ollama.New(ollama.WithBaseURL(host), ollama.WithModel(opt.Model))
	case "vllm":
		return vllm.New(vllm.WithBaseURL(envFirst("MAESTRO_VLLM")), vllm.WithModel(opt.Model))
	default:
		return nil, fmt.Errorf("unknown provider %q", opt.ProviderName)
	}
}

// friendlyErr renders a chat error in a form the UI can show without
// leaking secrets or call-site detail. The toolkit's typed error model
// makes this easy: provider-classified errors stringify cleanly.
func friendlyErr(err error) string {
	var pe *llms.ProviderError
	if errors.As(err, &pe) {
		return fmt.Sprintf("%s: %s", pe.Kind, pe.Message)
	}
	return err.Error()
}

func findOption(options []ModelOption, id string) (ModelOption, bool) {
	for _, opt := range options {
		if opt.ID == id {
			return opt, true
		}
	}
	return ModelOption{}, false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		// We've already started writing the response; just log.
		_ = err
	}
}

// estimateTokensForDisplay is a thin wrapper around
// llms.EstimateTextTokens used by the UI to show "approx tokens" before
// sending. The UI calls /api/chat for completion; if/when we wanted a
// purely-client-side estimator we could expose a tiny /api/estimate
// endpoint, but for the demo the chat round-trip surfaces actual usage
// (resp.Usage) so a pre-send estimate would be redundant.
//
// Kept here as a reference for developers reading the demo so they can
// see the helper and decide whether their own UI wants a pre-send
// estimate.
func estimateTokensForDisplay(text string) int {
	return llms.EstimateTextTokens(strings.TrimSpace(text))
}

// Ensure the helper is referenced so the import + symbol stay live for
// readers who care about the wiring. Without this, an unused-import
// lint would force us to either delete the demonstration or use the
// function in a way that distracts from the chat flow. Keeping a
// no-op compile-time touch is the least intrusive option.
var _ = estimateTokensForDisplay

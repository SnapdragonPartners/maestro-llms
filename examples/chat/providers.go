package main

// providers.go is the per-provider env detection + model picking glue.
// One entry per provider in the dropdown — we pick a sensible default
// family per hosted provider and surface its latest. For Ollama we pick
// the most-recently-pulled local model (what the developer is most
// likely actively iterating on); for vLLM we take the first model the
// server reports (vLLM usually serves exactly one).
//
// Every provider follows the same template:
//
//   1. Check env for the right credentials/endpoint; bail if missing.
//   2. Construct the chat client.
//   3. Call ListModels (under its own per-provider deadline so one slow
//      provider can't starve the others) and pick one model worth
//      surfacing.
//
// This file is intentionally short on abstraction so a developer can
// read it top-to-bottom and see exactly how each provider wires up.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/vllm"
)

// ModelOption is one entry in the dropdown the UI presents to the user.
// ID is a stable identifier (used as the <option value>); Label is what
// the user sees. ProviderName + Model are what we use server-side to
// rebuild the right ChatClient.
type ModelOption struct {
	ID           string `json:"id"`           // e.g. "anthropic/claude-opus-4-7-20251015"
	Label        string `json:"label"`        // e.g. "Anthropic — claude-opus-4-7-20251015"
	ProviderName string `json:"providerName"` // "anthropic"|"openai"|"google"|"ollama"|"vllm"
	Model        string `json:"model"`        // actual model ID to pass to WithModel
}

// envFirst returns the first non-empty environment variable from names.
// Used for providers that accept multiple key names (Anthropic, Google).
func envFirst(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// detectAvailable inspects the environment and returns one dropdown
// entry per reachable provider. Errors per-provider are logged but
// non-fatal: a missing key just means "that provider isn't in the
// dropdown today." A broken provider (e.g. vLLM unreachable) is also
// logged + skipped — we never want one provider's outage to take down
// the whole demo.
func detectAvailable(ctx context.Context, logf func(string, ...any)) []ModelOption {
	var out []ModelOption
	if opt, ok := anthropicOption(ctx, logf); ok {
		out = append(out, opt)
	}
	if opt, ok := openAIOption(ctx, logf); ok {
		out = append(out, opt)
	}
	if opt, ok := googleOption(ctx, logf); ok {
		out = append(out, opt)
	}
	if opt, ok := ollamaOption(ctx, logf); ok {
		out = append(out, opt)
	}
	if opt, ok := vllmOption(ctx, logf); ok {
		out = append(out, opt)
	}
	return out
}

// ----- Anthropic ----------------------------------------------------------

// anthropicPreferredFamilies are tried in order; the first family that
// has any models in the catalog provides the dropdown entry.
var anthropicPreferredFamilies = []string{"claude-opus", "claude-sonnet", "claude-haiku"}

func anthropicOption(ctx context.Context, logf func(string, ...any)) (ModelOption, bool) {
	key := envFirst("ANTHROPIC_API_KEY", "MAESTRO_ANTHROPIC_API_KEY")
	if key == "" {
		return ModelOption{}, false
	}
	// WithModel is required at construction; we use a known default just
	// to satisfy validation — we'll re-create the client per request with
	// the actually-selected model.
	c, err := anthropic.New(
		anthropic.WithAPIKey(key),
		anthropic.WithModel("claude-haiku-4-5-20251001"),
	)
	if err != nil {
		logf("anthropic: construct failed: %v", err)
		return ModelOption{}, false
	}
	lctx, cancel := withListTimeout(ctx)
	defer cancel()
	models, err := c.ListModels(lctx)
	if err != nil {
		logf("anthropic: ListModels failed: %v", err)
		return ModelOption{}, false
	}
	for _, fam := range anthropicPreferredFamilies {
		if newest, ok := newestByCreated(models, fam); ok {
			return modelOption("anthropic", "Anthropic", newest.ID), true
		}
	}
	return ModelOption{}, false
}

// ----- OpenAI ------------------------------------------------------------

// openAIPreferredFamilies are tried in order. We avoid the smaller
// "mini" variants here so the demo's default is the larger model for
// each generation — easy to tweak if you want a cheaper default.
var openAIPreferredFamilies = []string{"gpt-5", "gpt-4o", "o3"}

func openAIOption(ctx context.Context, logf func(string, ...any)) (ModelOption, bool) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return ModelOption{}, false
	}
	c, err := openai.NewChat(
		openai.WithAPIKey(key),
		openai.WithModel("gpt-4o-mini"),
	)
	if err != nil {
		logf("openai: construct failed: %v", err)
		return ModelOption{}, false
	}
	lctx, cancel := withListTimeout(ctx)
	defer cancel()
	models, err := c.ListModels(lctx)
	if err != nil {
		logf("openai: ListModels failed: %v", err)
		return ModelOption{}, false
	}
	for _, fam := range openAIPreferredFamilies {
		if newest, ok := newestByCreated(models, fam); ok {
			return modelOption("openai", "OpenAI", newest.ID), true
		}
	}
	return ModelOption{}, false
}

// ----- Google (Gemini) ---------------------------------------------------

var googlePreferredFamilies = []string{"gemini-pro", "gemini-flash"}

func googleOption(ctx context.Context, logf func(string, ...any)) (ModelOption, bool) {
	key := envFirst("GEMINI_API_KEY", "GOOGLE_GENAI_API_KEY", "GOOGLE_API_KEY")
	if key == "" {
		return ModelOption{}, false
	}
	c, err := google.New(
		google.WithAPIKey(key),
		google.WithModel("gemini-2.5-flash"),
	)
	if err != nil {
		logf("google: construct failed: %v", err)
		return ModelOption{}, false
	}
	lctx, cancel := withListTimeout(ctx)
	defer cancel()
	models, err := c.ListModels(lctx)
	if err != nil {
		logf("google: ListModels failed: %v", err)
		return ModelOption{}, false
	}
	for _, fam := range googlePreferredFamilies {
		// Gemini's ListModels does not expose Created; order by the
		// numeric version embedded in the ID, matching what the
		// google.LatestInFamily helper does internally.
		if newest, ok := newestByGeminiVersion(models, fam); ok {
			return modelOption("google", "Google", newest.ID), true
		}
	}
	return ModelOption{}, false
}

// ----- Ollama (local) -----------------------------------------------------

func ollamaOption(ctx context.Context, logf func(string, ...any)) (ModelOption, bool) {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://localhost:11434"
	}
	c, err := ollama.New(
		ollama.WithBaseURL(host),
		ollama.WithModel("placeholder"), // re-created per request
	)
	if err != nil {
		logf("ollama: construct failed: %v", err)
		return ModelOption{}, false
	}
	lctx, cancel := withListTimeout(ctx)
	defer cancel()
	models, err := c.ListModels(lctx)
	if err != nil {
		// Likely just "ollama not running" — common dev case, log quietly.
		logf("ollama: ListModels failed (server probably not running): %v", err)
		return ModelOption{}, false
	}
	if len(models) == 0 {
		return ModelOption{}, false
	}
	// Pick whichever model has been most recently pulled — that's the
	// one the developer is most likely actively iterating on. Tie-break
	// by lexical ID descending so the choice is deterministic when two
	// models share a Created time (mirrors the toolkit LatestInFamily
	// helpers' tie-break).
	newest := models[0]
	for _, m := range models {
		if m.Created.After(newest.Created) || (m.Created.Equal(newest.Created) && m.ID > newest.ID) {
			newest = m
		}
	}
	return modelOption("ollama", "Ollama", newest.ID), true
}

// ----- vLLM (self-hosted) -------------------------------------------------

func vllmOption(ctx context.Context, logf func(string, ...any)) (ModelOption, bool) {
	base := os.Getenv("MAESTRO_VLLM")
	if base == "" {
		return ModelOption{}, false
	}
	c, err := vllm.New(
		vllm.WithBaseURL(base),
		vllm.WithModel("placeholder"),
	)
	if err != nil {
		logf("vllm: construct failed: %v", err)
		return ModelOption{}, false
	}
	lctx, cancel := withListTimeout(ctx)
	defer cancel()
	models, err := c.ListModels(lctx)
	if err != nil {
		logf("vllm: ListModels failed: %v", err)
		return ModelOption{}, false
	}
	if len(models) == 0 {
		return ModelOption{}, false
	}
	// vLLM usually serves exactly one model; if multiple, take the first.
	return modelOption("vllm", "vLLM", models[0].ID), true
}

// ----- helpers ------------------------------------------------------------

// modelOption builds the dropdown entry for a chosen provider+model.
func modelOption(providerName, vendorLabel, modelID string) ModelOption {
	return ModelOption{
		ID:           providerName + "/" + modelID,
		Label:        fmt.Sprintf("%s — %s", vendorLabel, modelID),
		ProviderName: providerName,
		Model:        modelID,
	}
}

// newestByCreated returns the newest ModelInfo whose Family matches fam,
// ordered by Created descending with a lexical-ID tie-break. The
// tie-break mirrors what the toolkit's LatestInFamily helpers do (e.g.
// anthropic.LatestInFamily), so a catalog returning equal or zero
// creation times produces a deterministic, stable selection rather than
// leaking API ordering into the dropdown.
func newestByCreated(models []llms.ModelInfo, fam string) (llms.ModelInfo, bool) {
	var (
		newest llms.ModelInfo
		found  bool
	)
	for _, m := range models {
		if m.Family != fam {
			continue
		}
		switch {
		case !found:
			newest = m
			found = true
		case m.Created.After(newest.Created):
			newest = m
		case m.Created.Equal(newest.Created) && m.ID > newest.ID:
			newest = m
		}
	}
	return newest, found
}

// newestByGeminiVersion orders Gemini models by the numeric version
// embedded in the ID (gemini-3-pro > gemini-2.5-pro > gemini-1.5-pro)
// since the genai list does not expose Created. Matches the ordering
// google.LatestInFamily does internally.
func newestByGeminiVersion(models []llms.ModelInfo, fam string) (llms.ModelInfo, bool) {
	var (
		newest llms.ModelInfo
		bestV  float64 = -1
	)
	for _, m := range models {
		if m.Family != fam {
			continue
		}
		v := versionOfGeminiID(m.ID)
		if v > bestV || (v == bestV && m.ID > newest.ID) {
			newest = m
			bestV = v
		}
	}
	if bestV < 0 {
		return llms.ModelInfo{}, false
	}
	return newest, true
}

// versionOfGeminiID extracts the numeric version segment from a Gemini
// model ID: "gemini-1.5-pro-001" → 1.5; "gemini-3-pro-preview" → 3.
// Returns 0 if no version is parseable (legacy IDs like "gemini-pro").
func versionOfGeminiID(id string) float64 {
	const prefix = "gemini-"
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	rest := id[len(prefix):]
	end := strings.IndexFunc(rest, func(r rune) bool {
		return r != '.' && (r < '0' || r > '9')
	})
	if end <= 0 {
		return 0
	}
	v, err := strconv.ParseFloat(rest[:end], 64)
	if err != nil {
		return 0
	}
	return v
}

// listTimeout caps how long we wait on each provider's ListModels during
// dropdown population. We never want a slow / dead provider to block the
// demo from starting up.
const listTimeout = 8 * time.Second

// withListTimeout wraps ctx with listTimeout when ctx does not already
// carry a tighter deadline.
func withListTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) < listTimeout {
		return parent, func() {}
	}
	return context.WithTimeout(parent, listTimeout)
}

package google

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// resourcePrefix is the genai resource-path prefix on Model.Name. We strip
// it so ModelInfo.ID is directly usable as WithModel(newer.ID).
const resourcePrefix = "models/"

// familyRe matches the Gemini role token (`pro`/`flash`/`nano`/`ultra`)
// in a stripped model ID. The role rides at or near the end after the
// version: `gemini-1.5-pro-001`, `gemini-3-pro-preview`,
// `gemini-2.0-flash`. See ADR-0012 for the permissive-by-default
// rationale.
var familyRe = regexp.MustCompile(`(?i)\bgemini-(?:[\d.]+-)?(pro|flash|nano|ultra)\b`)

// versionRe extracts a numeric version segment from a stripped model ID:
// `gemini-1.5-pro-001` → "1.5"; `gemini-3-pro-preview` → "3";
// `gemini-2.0-flash` → "2.0". Used because the genai list does not expose
// a created date, so "newer" must come from the ID itself.
var versionRe = regexp.MustCompile(`(?i)gemini-(\d+(?:\.\d+)?)-`)

// stripPrefix removes the resource-path prefix Gemini returns on list.
func stripPrefix(name string) string {
	return strings.TrimPrefix(name, resourcePrefix)
}

// familyOf returns `gemini-{pro|flash|nano|ultra}` if id matches a known
// Gemini role token; otherwise "". Crosses major versions: 1.5-pro,
// 2.0-pro, 3-pro all → `gemini-pro`.
func familyOf(id string) string {
	id = stripPrefix(id)
	m := familyRe.FindStringSubmatch(id)
	if len(m) < 2 {
		return ""
	}
	return "gemini-" + strings.ToLower(m[1])
}

// versionOf returns the numeric version embedded in a Gemini model ID,
// or 0 if none is parseable (e.g. legacy `gemini-pro`). Used for
// "newer than" ordering within a family — genai exposes no Created date
// on Model.List, so the ID itself is the only ordering signal.
func versionOf(id string) float64 {
	id = stripPrefix(id)
	m := versionRe.FindStringSubmatch(id)
	if len(m) < 2 {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	return v
}

// ListModels returns the catalog the genai SDK exposes. Implements
// llms.ModelLister. ModelInfo.Created is left zero because the genai
// list does not expose a creation timestamp.
func (c *Client) ListModels(ctx context.Context) ([]llms.ModelInfo, error) {
	// Size is unknown up front (iterator); a small initial capacity avoids
	// the early-append re-grows for the common case (a few dozen models).
	out := make([]llms.ModelInfo, 0, 32)
	for m, err := range c.api.Models.All(ctx) {
		if err != nil {
			return nil, classifyError(c.model, err)
		}
		id := stripPrefix(m.Name)
		out = append(out, llms.ModelInfo{
			ID:     id,
			Family: familyOf(id),
			// Created intentionally zero — see ADR-0012.
			Raw: m,
		})
	}
	return out, nil
}

// LatestInFamily returns (newer, true) only when models contains a model
// whose family matches currentID's AND whose parsed version is strictly
// greater than currentID's. Returns (zero, false) otherwise.
//
// Ordering is by versionOf, since genai exposes no Created date; ties
// (e.g. two `gemini-2.0-flash-...` snapshots) break by lexical ID desc
// so the result is deterministic across calls.
//
// Pure helper (no I/O). For one-shot use see Client.LatestInFamily.
func LatestInFamily(currentID string, models []llms.ModelInfo) (llms.ModelInfo, bool) {
	fam := familyOf(currentID)
	if fam == "" {
		return llms.ModelInfo{}, false
	}
	fam2 := make([]llms.ModelInfo, 0, len(models))
	for i := range models {
		if models[i].Family != fam {
			continue
		}
		fam2 = append(fam2, models[i])
	}
	if len(fam2) == 0 {
		return llms.ModelInfo{}, false
	}
	sort.SliceStable(fam2, func(i, j int) bool {
		vi, vj := versionOf(fam2[i].ID), versionOf(fam2[j].ID)
		if vi != vj {
			return vi > vj
		}
		return fam2[i].ID > fam2[j].ID
	})
	newest := fam2[0]
	if newest.ID == currentID {
		return llms.ModelInfo{}, false
	}
	// Strictly newer by version (or by lex within same version).
	currentV := versionOf(currentID)
	newestV := versionOf(newest.ID)
	if newestV < currentV {
		return llms.ModelInfo{}, false
	}
	if newestV == currentV && newest.ID <= currentID {
		return llms.ModelInfo{}, false
	}
	return newest, true
}

// LatestInFamily is the one-shot convenience: lists + filters in one call.
func (c *Client) LatestInFamily(ctx context.Context, currentID string) (llms.ModelInfo, bool, error) {
	models, err := c.ListModels(ctx)
	if err != nil {
		return llms.ModelInfo{}, false, err
	}
	newer, ok := LatestInFamily(currentID, models)
	return newer, ok, nil
}

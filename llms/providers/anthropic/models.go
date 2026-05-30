package anthropic

import (
	"context"
	"regexp"
	"sort"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// familyRe matches the Anthropic-classifier word inside a model ID. Both
// new naming (claude-opus-4-7-20251015) and older generations
// (claude-3-5-sonnet-20240620, claude-3-opus-20240229) all carry one of
// these three tokens. See ADR-0012 for the permissive-by-default
// rationale.
var familyRe = regexp.MustCompile(`(?i)\b(opus|sonnet|haiku)\b`)

// familyOf returns "claude-{opus|sonnet|haiku}" if id matches the
// known pattern, otherwise "". Cross-generation by design — both
// claude-3-5-sonnet-... and claude-sonnet-4-5-... resolve to
// "claude-sonnet", matching the upgrade-detection use case.
func familyOf(id string) string {
	m := familyRe.FindString(id)
	if m == "" {
		return ""
	}
	// Lower-case the matched token so case-insensitive matches still
	// normalize to a single family string.
	out := []byte("claude-")
	for i := 0; i < len(m); i++ {
		c := m[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// ListModels returns the catalog the Anthropic API exposes via
// /v1/models, auto-paging through cursors. Implements llms.ModelLister.
func (c *Client) ListModels(ctx context.Context) ([]llms.ModelInfo, error) {
	iter := c.api.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	var out []llms.ModelInfo
	for iter.Next() {
		m := iter.Current()
		out = append(out, llms.ModelInfo{
			ID:      m.ID,
			Family:  familyOf(m.ID),
			Created: m.CreatedAt,
			Raw:     m,
		})
	}
	if err := iter.Err(); err != nil {
		return nil, classifyError(c.model, err)
	}
	return out, nil
}

// LatestInFamily returns (newer, true) only when models contains a model
// whose family matches currentID's AND whose CreatedAt is strictly later
// than currentID's own entry (if present). Returns (zero, false) when
// currentID has no parseable family, no other model shares it, or
// currentID is already the newest in its family.
//
// This is a pure helper (no I/O); apps may cache a ListModels result and
// call this repeatedly. For one-shot use, see Client.LatestInFamily.
func LatestInFamily(currentID string, models []llms.ModelInfo) (llms.ModelInfo, bool) {
	fam := familyOf(currentID)
	if fam == "" {
		return llms.ModelInfo{}, false
	}
	// Collect family matches and find the current entry (if listed) so
	// we can compare timestamps. The current ID may legitimately be
	// missing from the catalog (deprecated, regional, internal alias);
	// in that case we still return the newest known family member as
	// long as it isn't the current ID itself.
	fam2 := make([]llms.ModelInfo, 0, len(models))
	currentT := zeroIfAbsent(currentID, models)
	for i := range models {
		m := models[i]
		if m.Family != fam {
			continue
		}
		fam2 = append(fam2, m)
	}
	if len(fam2) == 0 {
		return llms.ModelInfo{}, false
	}
	// Sort newest first by CreatedAt desc, with ID as a deterministic
	// tiebreak so ties don't flip across calls.
	sort.SliceStable(fam2, func(i, j int) bool {
		if !fam2[i].Created.Equal(fam2[j].Created) {
			return fam2[i].Created.After(fam2[j].Created)
		}
		return fam2[i].ID > fam2[j].ID
	})
	newest := fam2[0]
	if newest.ID == currentID {
		return llms.ModelInfo{}, false
	}
	// Strictly newer than currentID's own entry, where we have one.
	if !currentT.IsZero() && !newest.Created.After(currentT) {
		return llms.ModelInfo{}, false
	}
	return newest, true
}

// LatestInFamily is the one-shot convenience: fetches the model list and
// runs the pure-function check in one call. Apps doing repeated checks
// should cache the ListModels result and call the package-level
// LatestInFamily helper instead of paying for ListModels each time.
func (c *Client) LatestInFamily(ctx context.Context, currentID string) (llms.ModelInfo, bool, error) {
	models, err := c.ListModels(ctx)
	if err != nil {
		return llms.ModelInfo{}, false, err
	}
	newer, ok := LatestInFamily(currentID, models)
	return newer, ok, nil
}

// zeroIfAbsent finds currentID's CreatedAt in models, or returns zero if
// it's not present (deprecated, alias, regional, etc.).
func zeroIfAbsent(currentID string, models []llms.ModelInfo) time.Time {
	for i := range models {
		if models[i].ID == currentID {
			return models[i].Created
		}
	}
	return time.Time{}
}

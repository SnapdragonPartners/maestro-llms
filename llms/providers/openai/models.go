package openai

import (
	"context"
	"regexp"
	"sort"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// dateSuffixRe matches a trailing `-YYYY-MM-DD` snapshot suffix on OpenAI
// model IDs. Stripping it yields the family. See ADR-0012.
var dateSuffixRe = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

// familyOf returns the OpenAI model ID with any trailing dated snapshot
// suffix removed: `gpt-5-2026-03-15` → `gpt-5`; `gpt-4o-mini-2024-07-18`
// → `gpt-4o-mini`; an ID with no date suffix is its own family.
//
// This is intentionally permissive: every dated snapshot in the same
// model line shares a family, so `LatestInFamily` on
// `gpt-5-2026-01-01` finds the newer `gpt-5-2026-03-15`. Non-chat
// models (embeddings, image, audio) also get classified — they self-
// filter by family-prefix when callers query a chat model, so they
// never collide.
func familyOf(id string) string {
	return dateSuffixRe.ReplaceAllString(id, "")
}

// ListModels returns every model the OpenAI account can access. Implements
// llms.ModelLister.
//
// NOTE: The OpenAI list endpoint returns every model the key can access —
// chat, embeddings, image, audio, fine-tunes, deprecated. The toolkit
// deliberately does not filter (see ADR-0012): LatestInFamily is
// self-filtering by family-prefix, and callers wanting only chat-capable
// models filter the result themselves (`gpt-`, `o1`, `o3`, … prefixes).
func (c *ChatClient) ListModels(ctx context.Context) ([]llms.ModelInfo, error) {
	page, err := c.api.Models.List(ctx)
	if err != nil {
		return nil, classifyError(c.model, err)
	}
	out := make([]llms.ModelInfo, 0, len(page.Data))
	for i := range page.Data {
		m := page.Data[i]
		var created time.Time
		if m.Created > 0 {
			created = time.Unix(m.Created, 0).UTC()
		}
		out = append(out, llms.ModelInfo{
			ID:      m.ID,
			Family:  familyOf(m.ID),
			Created: created,
			Raw:     m,
		})
	}
	return out, nil
}

// LatestInFamily returns (newer, true) only when models contains a model
// whose family matches currentID's AND whose Created is strictly later
// than currentID's own entry (if present). Returns (zero, false) when
// currentID is already the newest in its family, no family member is
// listed, or models is empty.
//
// Pure helper (no I/O). For one-shot use see ChatClient.LatestInFamily.
func LatestInFamily(currentID string, models []llms.ModelInfo) (llms.ModelInfo, bool) {
	fam := familyOf(currentID)
	if fam == "" {
		return llms.ModelInfo{}, false
	}
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
	if !currentT.IsZero() && !newest.Created.After(currentT) {
		return llms.ModelInfo{}, false
	}
	return newest, true
}

// LatestInFamily is the one-shot convenience: fetches the catalog and
// runs the pure-function check in one call. Apps making repeated checks
// should cache the ListModels result and call the package-level helper.
func (c *ChatClient) LatestInFamily(ctx context.Context, currentID string) (llms.ModelInfo, bool, error) {
	models, err := c.ListModels(ctx)
	if err != nil {
		return llms.ModelInfo{}, false, err
	}
	newer, ok := LatestInFamily(currentID, models)
	return newer, ok, nil
}

func zeroIfAbsent(currentID string, models []llms.ModelInfo) time.Time {
	for i := range models {
		if models[i].ID == currentID {
			return models[i].Created
		}
	}
	return time.Time{}
}

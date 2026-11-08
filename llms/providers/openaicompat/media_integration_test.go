//go:build integration

package openaicompat_test

import (
	"os"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms/internal/livetest"
)

// ADR-0040: images as data-URI image_url parts. Needs a vision model on the
// router, so it runs only with LUNAROUTE_VISION_MODEL set.
func TestIntegrationLunaRouteImageInput(t *testing.T) {
	key := lunaRouteKeyOrSkip(t)
	model := os.Getenv("LUNAROUTE_VISION_MODEL")
	if model == "" {
		t.Skip("LUNAROUTE_VISION_MODEL not set; skipping LunaRoute image test")
	}
	livetest.ImageInput(t, newLunaRoute(t, key, model))
}

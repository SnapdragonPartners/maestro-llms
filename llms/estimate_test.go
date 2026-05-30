package llms_test

import (
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

func TestEstimateTextTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		// 1-3 runes all round up to 1 token under ceil-divide.
		{"single rune", "a", 1},
		{"three ascii", "abc", 1},
		{"exactly four ascii", "abcd", 1},
		{"five ascii rounds up", "abcde", 2},
		{"eight ascii", "abcdefgh", 2},
		{"long ascii", strings.Repeat("x", 400), 100},
		// Multibyte runes count by rune, not byte — Greek alpha is 2 bytes.
		{"greek 4 runes = 1 token", "αβγδ", 1},
		{"greek 5 runes = 2 tokens", "αβγδε", 2},
		// CJK: 3 bytes per rune in UTF-8; byte-counting would massively
		// overestimate (12 bytes / 4 = 3) while rune-counting gives 1.
		{"cjk 4 runes = 1 token", "日本語あ", 1},
		// Mixed scripts: 4 ASCII + 4 CJK = 8 runes = 2 tokens.
		{"mixed 8 runes = 2 tokens", "abcd日本語あ", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := llms.EstimateTextTokens(tc.in); got != tc.want {
				t.Errorf("EstimateTextTokens(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// Compile-time assertion that EstimateTextTokens is usable as the
// `func(string) int` shape maestro-cms (ADR-0013 requester) consumes.
// If a future change widens the signature, compilation fails here.
var _ func(string) int = llms.EstimateTextTokens

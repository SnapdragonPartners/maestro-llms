package llms

import "unicode/utf8"

// EstimateTextTokens returns a fast, provider-neutral, char-based
// approximation of the token count of a standalone string. It is a free
// function (no estimator construction, no ModelRef needed) for the
// common case where a consumer is splitting text to a budget — e.g.
// chunking for embeddings — and just needs a `func(string) int`.
//
// Bias: NEUTRAL (~4 chars/token, rune-counted). This is intentionally
// different from the request-shaped middleware TokenEstimator
// (llms/middleware), which biases HIGH (~3 chars/token, byte-counted)
// because over-reservation is the safe error at a rate limiter. For
// chunking, an overestimate produces smaller-than-necessary chunks and
// thus more downstream API calls — wasteful, not unsafe — so neutral is
// the better default here. Consumers wanting a safety margin add it
// themselves (e.g. multiply the result, or shrink their budget).
//
// Why two estimators: the rate-limit middleware reserves capacity
// before a request and must not under-reserve; chunking divides text
// before any reservation exists and gains nothing from rounding up.
// Conflating them would force the wrong bias on at least one consumer.
// See ADR-0013.
//
// Rune-counted rather than byte-counted so the ratio holds for
// non-ASCII text: `len(s)` would underestimate token count for scripts
// where one rune spans multiple UTF-8 bytes (CJK, most non-Latin
// alphabets), which is the opposite of the limiter's intentional
// byte-based overestimate.
//
// Provider-neutral and zero-dependency. A tokenizer-backed,
// model-aware variant (opt-in, additive) may be added later; this is
// the minimal v1 helper, sufficient for the chunking use case the
// requester (maestro-cms) described.
//
// Returns 0 for the empty string.
func EstimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	const charsPerToken = 4
	runes := utf8.RuneCountInString(s)
	// Ceiling divide so a single character does not estimate to zero.
	return (runes + charsPerToken - 1) / charsPerToken
}

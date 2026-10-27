// Package satint is saturating int arithmetic for the toolkit's token
// accounting (ADR-0034). Token counts come from callers and providers, so
// estimate, reserve, commit and refill math must clamp at the int range
// instead of wrapping: a wrapped sum turns a huge charge into a refund, or a
// huge estimate into an admitted zero.
package satint

import "math"

// Add returns a + b, clamped to [math.MinInt, math.MaxInt].
func Add(a, b int) int {
	switch {
	case b > 0 && a > math.MaxInt-b:
		return math.MaxInt
	case b < 0 && a < math.MinInt-b:
		return math.MinInt
	}
	return a + b
}

// Sub returns a - b, clamped to [math.MinInt, math.MaxInt].
func Sub(a, b int) int {
	switch {
	case b < 0 && a > math.MaxInt+b:
		return math.MaxInt
	case b > 0 && a < math.MinInt+b:
		return math.MinInt
	}
	return a - b
}

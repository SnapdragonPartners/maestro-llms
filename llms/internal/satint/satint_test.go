package satint

import (
	"math"
	"testing"
)

func TestAddSub(t *testing.T) {
	cases := []struct{ a, b, add, sub int }{
		{1, 2, 3, -1},
		{math.MaxInt, 1, math.MaxInt, math.MaxInt - 1},
		{math.MaxInt, math.MaxInt, math.MaxInt, 0},
		{math.MinInt, -1, math.MinInt, math.MinInt + 1},
		{math.MinInt, 1, math.MinInt + 1, math.MinInt},
		{0, math.MinInt, math.MinInt, math.MaxInt},
		{-1, math.MinInt, math.MinInt, math.MaxInt},
		{math.MaxInt, math.MinInt, -1, math.MaxInt},
		{math.MinInt, math.MaxInt, -1, math.MinInt},
		{-5, 3, -2, -8},
	}
	for _, c := range cases {
		if got := Add(c.a, c.b); got != c.add {
			t.Errorf("Add(%d, %d) = %d, want %d", c.a, c.b, got, c.add)
		}
		if got := Sub(c.a, c.b); got != c.sub {
			t.Errorf("Sub(%d, %d) = %d, want %d", c.a, c.b, got, c.sub)
		}
	}
}

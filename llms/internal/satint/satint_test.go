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

func TestMul(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{0, math.MaxInt, 0},
		{3, 4, 12},
		{-3, 4, -12},
		{-3, -4, 12},
		{math.MaxInt, 2, math.MaxInt},
		{math.MaxInt, -2, math.MinInt},
		{math.MinInt, -1, math.MaxInt},
		{-1, math.MinInt, math.MaxInt},
		{math.MinInt, 1, math.MinInt},
		{math.MinInt, 2, math.MinInt},
		{math.MinInt, math.MinInt, math.MaxInt},
		{math.MaxInt/2 + 1, 2, math.MaxInt},
		{math.MinInt/2 - 1, 2, math.MinInt},
		{math.MaxInt / 2, 2, math.MaxInt - 1},
	}
	for _, c := range cases {
		if got := Mul(c.a, c.b); got != c.want {
			t.Errorf("Mul(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

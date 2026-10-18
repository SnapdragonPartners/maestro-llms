package llms

import "testing"

func TestCacheSplit(t *testing.T) {
	for _, tc := range []struct {
		name                string
		input, read, write  int
		wantRead, wantWrite int
	}{
		{"subsets kept", 100, 60, 30, 60, 30},
		{"no caching", 100, 0, 0, 0, 0},
		{"read clamped to input", 100, 150, 0, 100, 0},
		{"write clamped to the remainder", 100, 80, 50, 80, 20},
		{"negatives are zero", 100, -5, -1, 0, 0},
		{"negative input", -1, 10, 10, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w := CacheSplit(tc.input, tc.read, tc.write)
			if r != tc.wantRead || w != tc.wantWrite {
				t.Fatalf("CacheSplit(%d, %d, %d) = %d, %d; want %d, %d", tc.input, tc.read, tc.write, r, w, tc.wantRead, tc.wantWrite)
			}
		})
	}
}

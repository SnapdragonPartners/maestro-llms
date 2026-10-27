package bucket_test

import (
	"errors"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
)

func TestLimitsCapacity(t *testing.T) {
	cases := []struct {
		name string
		l    bucket.Limits
		want int
	}{
		{"off", bucket.Limits{}, 0},
		{"default buffer", bucket.Limits{TokensPerMinute: 1000}, 900},
		{"explicit buffer", bucket.Limits{TokensPerMinute: 1000, BufferFactor: 1}, 1000},
		{"tiny rate floors at 1", bucket.Limits{TokensPerMinute: 1}, 1},
	}
	for _, c := range cases {
		if got := c.l.Capacity(); got != c.want {
			t.Errorf("%s: Capacity() = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestLimitsValidate(t *testing.T) {
	bad := []bucket.Limits{
		{TokensPerMinute: -1},
		{MaxConcurrency: -1},
		{BufferFactor: -0.5},
		// ADR-0034: a non-finite factor is invalid even with tokens off.
		{BufferFactor: math.NaN()},
		{BufferFactor: math.Inf(1)},
		{BufferFactor: math.Inf(-1)},
		{TokensPerMinute: 10, BufferFactor: math.NaN()},
		// The computed capacity must fit in an int.
		{TokensPerMinute: math.MaxInt, BufferFactor: 1},
		{TokensPerMinute: math.MaxInt/2 + 1, BufferFactor: 2},
		{TokensPerMinute: 1000, BufferFactor: 1e300},
	}
	for _, l := range bad {
		if l.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want error", l)
		}
	}
	good := []bucket.Limits{
		{},
		{TokensPerMinute: 10, MaxConcurrency: 2},
		{BufferFactor: 1e300},          // tokens off: capacity is 0
		{TokensPerMinute: math.MaxInt}, // default 0.9 still fits
	}
	for _, l := range good {
		if err := l.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", l, err)
		}
	}
	if c := (bucket.Limits{TokensPerMinute: math.MaxInt}).Capacity(); c <= 0 {
		t.Errorf("Capacity at MaxInt TPM = %d, want positive", c)
	}
}

func TestUnknownKeyWraps(t *testing.T) {
	err := bucket.UnknownKey("k")
	if !errors.Is(err, bucket.ErrUnknownKey) || !strings.Contains(err.Error(), `"k"`) {
		t.Fatalf("UnknownKey = %v", err)
	}
}

// TestNoLLMImports enforces ADR-0025: the bucket packages are generic and
// must not import the llms packages. Every non-test file under bucket/ is
// checked, so the transitive closure within bucket is covered too.
func TestNoLLMImports(t *testing.T) {
	const forbidden = "github.com/SnapdragonPartners/maestro-llms/llms"
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == forbidden || strings.HasPrefix(p, forbidden+"/") {
				t.Errorf("%s imports %s; bucket must not import llms", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

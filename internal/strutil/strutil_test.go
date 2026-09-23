package strutil_test

import (
	"strings"
	"testing"

	"github.com/inful/mreview/internal/strutil"
)

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		// Under the cap: returned unchanged.
		{"empty", "", 10, ""},
		{"shorter than cap", "hello", 10, "hello"},
		{"equal to cap", "hello", 5, "hello"},

		// Over the cap: slice + ellipsis.
		{"one over", "hello!", 5, "hello..."},
		{"much longer", strings.Repeat("X", 100), 10, strings.Repeat("X", 10) + "..."},
		{"max zero returns empty + ellipsis", "anything", 0, "..."},
		{"negative max returns empty", "anything", -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strutil.Truncate(tc.s, tc.max)
			if got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.s, tc.max, got, tc.want)
			}
		})
	}
}

// TestTruncate_Boundary confirms a string exactly at the cap is
// returned as-is (no spurious ellipsis).
func TestTruncate_Boundary(t *testing.T) {
	s := strings.Repeat("a", 100)
	if got := strutil.Truncate(s, 100); got != s {
		t.Errorf("at-cap input should be unchanged, got %q (len %d)", got, len(got))
	}
}

// TestTruncate_BoundaryPlusOne confirms the first byte past the cap
// triggers truncation.
func TestTruncate_BoundaryPlusOne(t *testing.T) {
	s := strings.Repeat("a", 101)
	got := strutil.Truncate(s, 100)
	want := strings.Repeat("a", 100) + "..."
	if got != want {
		t.Errorf("over-cap input should be cut at 100 + ellipsis, got len %d", len(got))
	}
}

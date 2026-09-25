package bundled_test

import (
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/skills/bundled"
)

// TestBundled_HasExpectedSkills pins the names of the skills
// the binary ships with. A regression here (e.g. someone
// accidentally renaming or deleting a file) makes the
// assertion fail before the binary even ships — better than
// discovering "we lost `error-handling`" in a release post.
func TestBundled_HasExpectedSkills(t *testing.T) {
	got, err := bundled.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	sort.Strings(got)

	want := []string{
		"error-handling",
		"go-review",
		"testing-patterns",
		"tokensave-usage",
	}
	if !stringSliceEqual(got, want) {
		t.Errorf("bundled skill names = %v; want %v", got, want)
	}
}

// TestBundled_FS_ReadFile verifies that every named file is
// readable and non-empty. Catches the "someone committed an
// empty .md" failure mode where the embed succeeds but the
// skill is useless.
func TestBundled_FS_ReadFile(t *testing.T) {
	names, err := bundled.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			body, err := fs.ReadFile(bundled.FS(), name+".md")
			if err != nil {
				t.Fatalf("read %s.md: %v", name, err)
			}
			if len(body) == 0 {
				t.Errorf("%s.md is empty", name)
			}
			// Each skill should have a non-trivial
			// description (>= 50 chars after
			// frontmatter). A regression to a tiny
			// placeholder file would degrade the
			// list_skills UX.
			if len(body) < 100 {
				t.Errorf("%s.md suspiciously short: %d bytes", name, len(body))
			}
		})
	}
}

// TestBundled_DescribesEachFileAtTopLevel confirms every
// bundled skill has a description (frontmatter or first
// paragraph). The agent scans descriptions to decide which
// skills to read; a skill with no description is invisible
// in list_skills output.
func TestBundled_DescribesEachFileAtTopLevel(t *testing.T) {
	names, err := bundled.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			body, err := fs.ReadFile(bundled.FS(), name+".md")
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if firstNonEmptyLine(body) == "" {
				t.Errorf("%s.md has no first non-empty line", name)
			}
		})
	}
}

// stringSliceEqual returns true when a and b contain the same
// elements (order-sensitive, since we sort both sides
// upfront).
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// firstNonEmptyLine returns the first non-whitespace line of
// body, or "" if all lines are blank.
func firstNonEmptyLine(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

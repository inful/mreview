package reviewer

import (
	"testing"

	"github.com/inful/mreview/internal/gitlab"
)

func TestFilterChangesByPath_NoPatterns(t *testing.T) {
	in := []gitlab.ChangeFile{
		{NewPath: "a.go"},
		{NewPath: "b.go"},
	}
	out := filterChangesByPath(in, nil)
	if len(out) != 2 {
		t.Errorf("no patterns should be a no-op; got %d files", len(out))
	}
}

func TestFilterChangesByPath_ExactGlob(t *testing.T) {
	in := []gitlab.ChangeFile{
		{NewPath: "main.pb.go"}, // bare filename → matches *.pb.go
		{NewPath: "src/main.go"},
		{NewPath: "src/foo_test.go"},
	}
	out := filterChangesByPath(in, []string{"*.pb.go"})
	if len(out) != 2 {
		t.Errorf("expected 2 surviving, got %d", len(out))
	}
	for _, c := range out {
		if c.NewPath == "main.pb.go" {
			t.Error("main.pb.go should have been filtered")
		}
	}
}

func TestFilterChangesByPath_Doublestar(t *testing.T) {
	// `**/*.pb.go` should match any depth (doublestar's
	// distinguishing feature).
	in := []gitlab.ChangeFile{
		{NewPath: "a.pb.go"},
		{NewPath: "pkg/x/a.pb.go"},
		{NewPath: "pkg/x/y/b.pb.go"},
		{NewPath: "real.go"},
	}
	out := filterChangesByPath(in, []string{"**/*.pb.go"})
	if len(out) != 1 || out[0].NewPath != "real.go" {
		t.Errorf("expected only real.go to survive; got %+v", out)
	}
}

func TestFilterChangesByPath_VendorDirectory(t *testing.T) {
	// `**/vendor/**` matches vendor/ at any depth; `vendor/**`
	// would only match at the root.
	in := []gitlab.ChangeFile{
		{NewPath: "vendor/foo/bar.go"},
		{NewPath: "pkg/vendor/thing.go"},
		{NewPath: "src/main.go"},
	}
	out := filterChangesByPath(in, []string{"**/vendor/**"})
	got := paths(out)
	want := []string{"src/main.go"}
	if !equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilterChangesByPath_PrefixMatch(t *testing.T) {
	// `**/generated/**` matches anything under any generated/
	in := []gitlab.ChangeFile{
		{NewPath: "generated/code.go"},
		{NewPath: "src/main.go"},
		{NewPath: "x/generated/y.go"},
	}
	out := filterChangesByPath(in, []string{"**/generated/**"})
	got := paths(out)
	want := []string{"src/main.go"}
	if !equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilterChangesByPath_MalformedPattern(t *testing.T) {
	// An unmatched [ is a syntax error for filepath.Match. Be
	// conservative: don't drop files.
	in := []gitlab.ChangeFile{
		{NewPath: "main.go"},
	}
	out := filterChangesByPath(in, []string{"[bad"})
	if len(out) != 1 {
		t.Errorf("malformed pattern should drop nothing; got %d", len(out))
	}
}

func TestMatchesAny_Basics(t *testing.T) {
	cases := []struct {
		path    string
		pattern string
		want    bool
	}{
		{"foo.pb.go", "*.pb.go", true},
		{"foo.go", "*.pb.go", false},
		{"a/b/c.pb.go", "**/*.pb.go", true},
		{"plain.go", "**/*.pb.go", false},
		{"vendor/x.go", "**/vendor/**", true},
		{"src/main.go", "**/vendor/**", false},
		{"pkg/vendor/x.go", "**/vendor/**", true},
		{"vendor/nested/deep.go", "vendor/**", true},
	}
	for _, tc := range cases {
		got := matchesAny(tc.path, []string{tc.pattern})
		if got != tc.want {
			t.Errorf("matchesAny(%q, %q) = %v, want %v", tc.path, tc.pattern, got, tc.want)
		}
	}
}

// helpers — local so tests don't pollute the package namespace.
func paths(in []gitlab.ChangeFile) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = c.NewPath
	}
	return out
}

func equal(a, b []string) bool {
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

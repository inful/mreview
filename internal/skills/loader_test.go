package skills

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
)

// fakeFetcher is a SkillFetcher implementation for tests.
// Records call counts and serves canned responses.
//
// The zero value is usable: with files=nil and listErr=nil the
// fetcher returns an empty list and a nil body for any path.
// Tests that need specific responses set the corresponding
// fields before passing the fetcher to New.
type fakeFetcher struct {
	listResp []gitlab.TreeNode
	listErr  error
	files    map[string][]byte
	fileErr  error // returned by GetRepositoryFileRaw for every path

	listCalls  int
	fetchCalls int

	// lastListProject / lastListPath / lastListRef record the
	// arguments of the most recent ListRepositoryTree call so
	// tests can assert wiring without spying on a mock library.
	lastListProject string
	lastListPath    string
	lastListRef     string

	// lastFetchProject / lastFetchPath / lastFetchRef mirror
	// lastList* for the most recent GetRepositoryFileRaw call.
	lastFetchProject string
	lastFetchPath    string
	lastFetchRef     string
}

func (f *fakeFetcher) ListRepositoryTree(_ context.Context, project, path, ref string) ([]gitlab.TreeNode, error) {
	f.listCalls++
	f.lastListProject = project
	f.lastListPath = path
	f.lastListRef = ref
	return f.listResp, f.listErr
}

func (f *fakeFetcher) GetRepositoryFileRaw(_ context.Context, project, p, ref string) ([]byte, error) {
	f.fetchCalls++
	f.lastFetchProject = project
	f.lastFetchPath = p
	f.lastFetchRef = ref
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	return f.files[p], nil
}

// TestLoad_FiltersToMarkdownBlobs verifies that Load includes
// only blobs whose Name ends in ".md". Subdirectories, regular
// files without .md, and other tree types are skipped silently.
func TestLoad_FiltersToMarkdownBlobs(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "abc", Name: "go-review.md", Path: "skills/go-review.md", Type: "blob", Mode: "100644"},
			{ID: "def", Name: "tokensave-usage.md", Path: "skills/tokensave-usage.md", Type: "blob", Mode: "100644"},
			{ID: "ghi", Name: "images", Path: "skills/images", Type: "tree", Mode: "040000"},         // dir, skipped
			{ID: "jkl", Name: "README.txt", Path: "skills/README.txt", Type: "blob", Mode: "100644"}, // not .md, skipped
			{ID: "mno", Name: "data.json", Path: "skills/data.json", Type: "blob", Mode: "100644"},   // not .md, skipped
		},
		files: map[string][]byte{
			"skills/go-review.md":       []byte("# go-review\n\nBody."),
			"skills/tokensave-usage.md": []byte("# tokensave-usage\n\nBody."),
		},
	}

	l := New(fetcher, "inful/mreview-skills", "skills", "main", nil)
	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(skills) = %d; want 2 (filtered). got=%+v", len(got), got)
	}
	// Names should match the trimmed filenames.
	gotNames := skillNames(got)
	wantNames := map[string]bool{"go-review": true, "tokensave-usage": true}
	if !equalSets(gotNames, wantNames) {
		t.Errorf("names = %v; want %v", gotNames, wantNames)
	}
	// Bodies should be populated.
	for _, s := range got {
		if len(s.Body) == 0 {
			t.Errorf("skill %q has empty body", s.Name)
		}
		if s.Path == "" {
			t.Errorf("skill %q has empty Path", s.Name)
		}
		if s.SHA == "" {
			t.Errorf("skill %q has empty SHA", s.Name)
		}
	}
}

// TestLoad_FetchesEachFileBody verifies that Load's second pass
// fetches every discovered markdown blob (one fetch per file).
func TestLoad_FetchesEachFileBody(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "a.md", Path: "skills/a.md", Type: "blob"},
			{ID: "2", Name: "b.md", Path: "skills/b.md", Type: "blob"},
			{ID: "3", Name: "c.md", Path: "skills/c.md", Type: "blob"},
		},
		files: map[string][]byte{
			"skills/a.md": []byte("body-a"),
			"skills/b.md": []byte("body-b"),
			"skills/c.md": []byte("body-c"),
		},
	}

	l := New(fetcher, "group/repo", "skills", "main", nil)
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if fetcher.fetchCalls != 3 {
		t.Errorf("fetchCalls = %d; want 3 (one per file)", fetcher.fetchCalls)
	}
}

// TestLoad_CachesSecondCall verifies that a second Load does not
// re-hit GitLab. The cache is process-lifetime.
func TestLoad_CachesSecondCall(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "a.md", Path: "skills/a.md", Type: "blob"},
		},
		files: map[string][]byte{"skills/a.md": []byte("body")},
	}

	l := New(fetcher, "group/repo", "skills", "main", nil)

	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("second Load: %v", err)
	}

	if fetcher.listCalls != 1 {
		t.Errorf("listCalls = %d; want 1 (second Load should hit cache)", fetcher.listCalls)
	}
	if fetcher.fetchCalls != 1 {
		t.Errorf("fetchCalls = %d; want 1 (second Load should hit cache)", fetcher.fetchCalls)
	}
}

// TestLoad_EmptyDirectoryReturnsEmptySlice verifies that a
// directory with no .md files produces an empty result, not
// nil. Empty skill sets are a valid configuration; the MCP
// server should still start and return [] to list_skills.
func TestLoad_EmptyDirectoryReturnsEmptySlice(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "images", Path: "skills/images", Type: "tree"},
			{ID: "2", Name: "README.txt", Path: "skills/README.txt", Type: "blob"},
		},
	}

	l := New(fetcher, "group/repo", "skills", "main", nil)
	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil; want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d; want 0", len(got))
	}
}

// TestLoad_ListErrorIsFatal verifies that a ListRepositoryTree
// failure surfaces as a Load error. Without the tree the loader
// has no idea what skills exist; silently returning [] would be
// a worse failure mode than the error.
func TestLoad_ListErrorIsFatal(t *testing.T) {
	listErr := errors.New("network down")
	fetcher := &fakeFetcher{listErr: listErr}

	l := New(fetcher, "group/repo", "skills", "main", nil)
	_, err := l.Load(context.Background())
	if err == nil {
		t.Fatal("expected error when list fails")
	}
	if !errors.Is(err, listErr) {
		t.Errorf("error chain should contain listErr; got %v", err)
	}
}

// TestLoad_SkipsFilesThatFailToFetch verifies that one bad file
// doesn't blank out the whole skill set. The bad file is logged
// and skipped; good files make it into the result.
func TestLoad_SkipsFilesThatFailToFetch(t *testing.T) {
	badPath := "skills/broken.md"
	goodPath := "skills/good.md"
	fetcher := &pathAwareFetcher{
		list: []gitlab.TreeNode{
			{ID: "1", Name: "broken.md", Path: badPath, Type: "blob"},
			{ID: "2", Name: "good.md", Path: goodPath, Type: "blob"},
		},
		files:  map[string][]byte{goodPath: []byte("good body")},
		failOn: map[string]error{badPath: errors.New("503 transient")},
	}

	l := New(fetcher, "group/repo", "skills", "main", nil)
	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d; want 1 (the good file). got=%+v", len(got), got)
	}
	if got[0].Name != "good" {
		t.Errorf("survivor = %q; want good", got[0].Name)
	}
}

// pathAwareFetcher is a SkillFetcher that returns a per-path
// error from GetRepositoryFileRaw. Used by
// TestLoad_SkipsFilesThatFailToFetch.
type pathAwareFetcher struct {
	files  map[string][]byte
	failOn map[string]error
	list   []gitlab.TreeNode
}

func (f *pathAwareFetcher) ListRepositoryTree(_ context.Context, _, _, _ string) ([]gitlab.TreeNode, error) {
	return f.list, nil
}

func (f *pathAwareFetcher) GetRepositoryFileRaw(_ context.Context, _, p, _ string) ([]byte, error) {
	if err, ok := f.failOn[p]; ok {
		return nil, err
	}
	return f.files[p], nil
}

// TestLoad_PassesRefToTransport verifies that Load forwards the
// configured ref to both transport methods. Ref-string handling
// lives in the transport; the loader's job is to pass it through.
func TestLoad_PassesRefToTransport(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "a.md", Path: "skills/a.md", Type: "blob"},
		},
		files: map[string][]byte{"skills/a.md": []byte("body")},
	}

	l := New(fetcher, "group/repo", "skills", "v1.2.3", nil)
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fetcher.lastListRef != "v1.2.3" {
		t.Errorf("list ref = %q; want v1.2.3", fetcher.lastListRef)
	}
	if fetcher.lastFetchRef != "v1.2.3" {
		t.Errorf("fetch ref = %q; want v1.2.3", fetcher.lastFetchRef)
	}
}

// TestLoad_PassesProjectAndDirectoryToTransport verifies that
// Load forwards the configured project + directory to
// ListRepositoryTree. Same plumbing concern as the ref test.
func TestLoad_PassesProjectAndDirectoryToTransport(t *testing.T) {
	fetcher := &fakeFetcher{}
	l := New(fetcher, "inful/mreview-skills", "skills", "main", nil)

	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fetcher.lastListProject != "inful/mreview-skills" {
		t.Errorf("list project = %q; want inful/mreview-skills", fetcher.lastListProject)
	}
	if fetcher.lastListPath != "skills" {
		t.Errorf("list path = %q; want skills", fetcher.lastListPath)
	}
}

// TestRead_AfterLoad_ReturnsSkill verifies the happy path:
// Load then Read by name returns the cached skill.
func TestRead_AfterLoad_ReturnsSkill(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "go-review.md", Path: "skills/go-review.md", Type: "blob"},
		},
		files: map[string][]byte{"skills/go-review.md": []byte("go body")},
	}

	l := New(fetcher, "group/repo", "skills", "main", nil)
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := l.Read(context.Background(), "go-review")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got.Body) != "go body" {
		t.Errorf("body = %q; want %q", got.Body, "go body")
	}
	if got.Name != "go-review" {
		t.Errorf("name = %q; want go-review", got.Name)
	}
}

// TestRead_UnknownName_ReturnsErrSkillNotFound verifies that
// reading a name not in the cache returns ErrSkillNotFound,
// distinct from a transport error. errors.Is must work.
func TestRead_UnknownName_ReturnsErrSkillNotFound(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "go-review.md", Path: "skills/go-review.md", Type: "blob"},
		},
		files: map[string][]byte{"skills/go-review.md": []byte("go body")},
	}

	l := New(fetcher, "group/repo", "skills", "main", nil)
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err := l.Read(context.Background(), "no-such-skill")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("err = %v; want ErrSkillNotFound", err)
	}
	// The error message should carry the requested name for debug logs.
	if !strings.Contains(err.Error(), "no-such-skill") {
		t.Errorf("error should name the requested skill; got %v", err)
	}
}

// TestRead_BeforeLoad_ReturnsErrSkillNotFound verifies that
// Read before Load fails cleanly. The cache is empty, so any
// name is unknown.
func TestRead_BeforeLoad_ReturnsErrSkillNotFound(t *testing.T) {
	fetcher := &fakeFetcher{}
	l := New(fetcher, "group/repo", "skills", "main", nil)

	_, err := l.Read(context.Background(), "anything")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("err = %v; want ErrSkillNotFound (no Load has run)", err)
	}
}

// TestExtractDescription covers the description-extraction
// helper. Table-driven — the rule is "first non-empty paragraph,
// trimmed, capped at descriptionCap".
func TestExtractDescription(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "frontmatter_then_paragraph",
			body: "---\nauthor: jone\n---\n\n# Heading\n\nBody line one.\nBody line two.\n",
			want: "# Heading",
		},
		{
			name: "single_line",
			body: "Just one line.",
			want: "Just one line.",
		},
		{
			name: "blank_lines_then_paragraph",
			body: "\n\n\nFirst paragraph.\n\nSecond paragraph.",
			want: "First paragraph.",
		},
		{
			name: "all_blank",
			body: "\n\n\n",
			want: "",
		},
		{
			name: "cap_at_200",
			body: strings.Repeat("a", 500),
			want: strings.Repeat("a", 200),
		},
		{
			name: "empty",
			body: "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractDescription([]byte(tc.body))
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// skillNames returns the set of Names in a Skill slice. Used by
// tests that don't care about order.
func skillNames(ss []Skill) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[s.Name] = true
	}
	return out
}

// equalSets returns true when a and b contain the same keys.
func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

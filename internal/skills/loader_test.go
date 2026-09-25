package skills

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/inful/mreview/internal/gitlab"
)

// fakeFetcher is a SkillFetcher implementation for tests.
// Records call counts and serves canned responses.
type fakeFetcher struct {
	listResp []gitlab.TreeNode
	listErr  error
	files    map[string][]byte
	fileErr  error // returned by GetRepositoryFileRaw for every path

	listCalls  int
	fetchCalls int

	lastListProject string
	lastListPath    string
	lastListRef     string

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

// pathAwareFetcher returns a per-path error from
// GetRepositoryFileRaw. Used by tests that need partial
// failures.
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

// bundled returns a small canned bundled map for tests. Names
// are stable so tests can assert presence by name.
func bundled() map[string][]byte {
	return map[string][]byte{
		"go-review":        []byte("# Go Review\n\nDefault Go checklist."),
		"testing-patterns": []byte("# Testing Patterns\n\nDefault test patterns."),
	}
}

func skillNames(ss []Skill) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[s.Name] = true
	}
	return out
}

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

// -----------------------------------------------------------------------------
// Remote-only behavior (existing tests, updated to Config API)
// -----------------------------------------------------------------------------

func TestLoad_FiltersToMarkdownBlobs(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "abc", Name: "go-review.md", Path: "skills/go-review.md", Type: "blob", Mode: "100644"},
			{ID: "def", Name: "tokensave-usage.md", Path: "skills/tokensave-usage.md", Type: "blob", Mode: "100644"},
			{ID: "ghi", Name: "images", Path: "skills/images", Type: "tree", Mode: "040000"},
			{ID: "jkl", Name: "README.txt", Path: "skills/README.txt", Type: "blob", Mode: "100644"},
			{ID: "mno", Name: "data.json", Path: "skills/data.json", Type: "blob", Mode: "100644"},
		},
		files: map[string][]byte{
			"skills/go-review.md":       []byte("# go-review\n\nBody."),
			"skills/tokensave-usage.md": []byte("# tokensave-usage\n\nBody."),
		},
	}

	l := New(Config{Fetcher: fetcher, RepoPath: "inful/mreview-skills", Directory: "skills", Ref: "main"})
	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(skills) = %d; want 2 (filtered). got=%+v", len(got), got)
	}
	gotNames := skillNames(got)
	wantNames := map[string]bool{"go-review": true, "tokensave-usage": true}
	if !equalSets(gotNames, wantNames) {
		t.Errorf("names = %v; want %v", gotNames, wantNames)
	}
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
		if s.IsBundled() {
			t.Errorf("remote skill %q should not be IsBundled()", s.Name)
		}
	}
}

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
	l := New(Config{Fetcher: fetcher, RepoPath: "group/repo", Directory: "skills", Ref: "main"})
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fetcher.fetchCalls != 3 {
		t.Errorf("fetchCalls = %d; want 3", fetcher.fetchCalls)
	}
}

func TestLoad_CachesSecondCall(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "a.md", Path: "skills/a.md", Type: "blob"},
		},
		files: map[string][]byte{"skills/a.md": []byte("body")},
	}
	l := New(Config{Fetcher: fetcher, RepoPath: "group/repo", Directory: "skills", Ref: "main"})
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if fetcher.listCalls != 1 {
		t.Errorf("listCalls = %d; want 1 (cached)", fetcher.listCalls)
	}
	if fetcher.fetchCalls != 1 {
		t.Errorf("fetchCalls = %d; want 1 (cached)", fetcher.fetchCalls)
	}
}

func TestLoad_EmptyDirectoryReturnsEmptySlice(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "images", Path: "skills/images", Type: "tree"},
			{ID: "2", Name: "README.txt", Path: "skills/README.txt", Type: "blob"},
		},
	}
	l := New(Config{Fetcher: fetcher, RepoPath: "group/repo", Directory: "skills", Ref: "main"})
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

func TestLoad_RemoteListErrorIsFatalWhenNoBundled(t *testing.T) {
	listErr := errors.New("network down")
	fetcher := &fakeFetcher{listErr: listErr}

	l := New(Config{Fetcher: fetcher, RepoPath: "group/repo", Directory: "skills", Ref: "main"})
	_, err := l.Load(context.Background())
	if err == nil {
		t.Fatal("expected error when list fails (no bundled fallback)")
	}
	if !errors.Is(err, listErr) {
		t.Errorf("error chain should contain listErr; got %v", err)
	}
}

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
	l := New(Config{Fetcher: fetcher, RepoPath: "group/repo", Directory: "skills", Ref: "main"})
	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d; want 1. got=%+v", len(got), got)
	}
	if got[0].Name != "good" {
		t.Errorf("survivor = %q; want good", got[0].Name)
	}
}

func TestLoad_PassesRefToTransport(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "a.md", Path: "skills/a.md", Type: "blob"},
		},
		files: map[string][]byte{"skills/a.md": []byte("body")},
	}
	l := New(Config{Fetcher: fetcher, RepoPath: "group/repo", Directory: "skills", Ref: "v1.2.3"})
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

func TestLoad_PassesProjectAndDirectoryToTransport(t *testing.T) {
	fetcher := &fakeFetcher{}
	l := New(Config{Fetcher: fetcher, RepoPath: "inful/mreview-skills", Directory: "skills", Ref: "main"})
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fetcher.lastListProject != "inful/mreview-skills" {
		t.Errorf("list project = %q", fetcher.lastListProject)
	}
	if fetcher.lastListPath != "skills" {
		t.Errorf("list path = %q", fetcher.lastListPath)
	}
}

func TestRead_AfterLoad_ReturnsSkill(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "go-review.md", Path: "skills/go-review.md", Type: "blob"},
		},
		files: map[string][]byte{"skills/go-review.md": []byte("go body")},
	}
	l := New(Config{Fetcher: fetcher, RepoPath: "group/repo", Directory: "skills", Ref: "main"})
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := l.Read(context.Background(), "go-review")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got.Body) != "go body" {
		t.Errorf("body = %q; want go body", got.Body)
	}
	if got.Name != "go-review" {
		t.Errorf("name = %q", got.Name)
	}
}

func TestRead_UnknownName_ReturnsErrSkillNotFound(t *testing.T) {
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "go-review.md", Path: "skills/go-review.md", Type: "blob"},
		},
		files: map[string][]byte{"skills/go-review.md": []byte("go body")},
	}
	l := New(Config{Fetcher: fetcher, RepoPath: "group/repo", Directory: "skills", Ref: "main"})
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err := l.Read(context.Background(), "no-such-skill")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("err = %v; want ErrSkillNotFound", err)
	}
	if !strings.Contains(err.Error(), "no-such-skill") {
		t.Errorf("error should name the requested skill; got %v", err)
	}
}

func TestRead_BeforeLoad_ReturnsErrSkillNotFound(t *testing.T) {
	l := New(Config{})
	_, err := l.Read(context.Background(), "anything")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("err = %v; want ErrSkillNotFound", err)
	}
}

func TestExtractDescription(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "frontmatter_then_paragraph", body: "---\nauthor: jone\n---\n\n# Heading\n\nBody line one.\nBody line two.\n", want: "# Heading"},
		{name: "single_line", body: "Just one line.", want: "Just one line."},
		{name: "blank_lines_then_paragraph", body: "\n\n\nFirst paragraph.\n\nSecond paragraph.", want: "First paragraph."},
		{name: "all_blank", body: "\n\n\n", want: ""},
		{name: "cap_at_200", body: strings.Repeat("a", 500), want: strings.Repeat("a", 200)},
		{name: "empty", body: "", want: ""},
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

// -----------------------------------------------------------------------------
// Bundled + Remote merge (issue #44 phase 2)
// -----------------------------------------------------------------------------

func TestLoad_BundledOnly_NoRemote(t *testing.T) {
	b := bundled()
	l := New(Config{Bundled: b})

	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(b) {
		t.Errorf("len = %d; want %d", len(got), len(b))
	}
	for _, s := range got {
		if !s.IsBundled() {
			t.Errorf("skill %q should be IsBundled()", s.Name)
		}
		if !strings.HasPrefix(s.Path, bundledPathPrefix) {
			t.Errorf("skill %q Path = %q; want %s prefix", s.Name, s.Path, bundledPathPrefix)
		}
		if s.SHA != "" {
			t.Errorf("bundled skill %q has non-empty SHA %q", s.Name, s.SHA)
		}
	}
}

func TestLoad_NoBundled_NoRemote_ReturnsEmpty(t *testing.T) {
	l := New(Config{})
	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d; want 0", len(got))
	}
}

func TestLoad_BundledPlusRemote_NoCollision(t *testing.T) {
	b := bundled() // {"go-review", "testing-patterns"}
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "api-design.md", Path: "skills/api-design.md", Type: "blob"},
		},
		files: map[string][]byte{"skills/api-design.md": []byte("API design notes.")},
	}
	l := New(Config{
		Fetcher:   fetcher,
		RepoPath:  "group/repo",
		Directory: "skills",
		Ref:       "main",
		Bundled:   b,
	})

	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d; want 3 (2 bundled + 1 remote). got=%+v", len(got), got)
	}

	byName := skillNames(got)
	want := map[string]bool{"go-review": true, "testing-patterns": true, "api-design": true}
	if !equalSets(byName, want) {
		t.Errorf("names = %v; want %v", byName, want)
	}

	// Bundled skills keep IsBundled=true when no collision.
	for _, s := range got {
		switch s.Name {
		case "go-review", "testing-patterns":
			if !s.IsBundled() {
				t.Errorf("bundled skill %q should be IsBundled()", s.Name)
			}
		case "api-design":
			if s.IsBundled() {
				t.Errorf("remote skill %q should NOT be IsBundled()", s.Name)
			}
		}
	}
}

func TestLoad_BundledPlusRemote_WithCollision_RemoteWins(t *testing.T) {
	// Bundled has "go-review" with body "bundled body".
	// Remote has "go-review" with body "remote body" + "new-skill".
	// After merge: go-review should be the remote version, new-skill
	// should be present, and the bundled go-review must NOT survive.
	b := bundled()
	b["go-review"] = []byte("# Bundled Go Review\n\nBundled body content.")
	fetcher := &fakeFetcher{
		listResp: []gitlab.TreeNode{
			{ID: "1", Name: "go-review.md", Path: "skills/go-review.md", Type: "blob"},
			{ID: "2", Name: "new-skill.md", Path: "skills/new-skill.md", Type: "blob"},
		},
		files: map[string][]byte{
			"skills/go-review.md": []byte("# Remote Go Review\n\nRemote body content."),
			"skills/new-skill.md": []byte("# New\n\nBrand new."),
		},
	}
	l := New(Config{
		Fetcher:   fetcher,
		RepoPath:  "group/repo",
		Directory: "skills",
		Ref:       "main",
		Bundled:   b,
	})

	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	byName := map[string]Skill{}
	for _, s := range got {
		byName[s.Name] = s
	}

	// 2 bundled (go-review overridden by remote, testing-patterns kept)
	// + 1 remote-only (new-skill) = 3 total.
	if len(got) != 3 {
		t.Errorf("len = %d; want 3. got=%+v", len(got), got)
	}

	gr, ok := byName["go-review"]
	if !ok {
		t.Fatal("go-review missing from result")
	}
	if gr.IsBundled() {
		t.Errorf("go-review should be remote after collision; got IsBundled=true")
	}
	if !strings.Contains(string(gr.Body), "Remote body content") {
		t.Errorf("go-review body should be the remote version; got %q", gr.Body)
	}
	if strings.Contains(string(gr.Body), "Bundled body content") {
		t.Errorf("bundled go-review body should not survive collision; got %q", gr.Body)
	}

	ns, ok := byName["new-skill"]
	if !ok {
		t.Fatal("new-skill missing from result")
	}
	if ns.IsBundled() {
		t.Errorf("new-skill should be remote (only exists in remote)")
	}
}

func TestLoad_RemoteFailureFallsBackToBundled(t *testing.T) {
	// Remote fetcher errors on ListRepositoryTree. Bundled
	// skills must still surface so the agent has the baseline.
	fetcher := &fakeFetcher{listErr: errors.New("network down")}
	b := bundled()
	l := New(Config{
		Fetcher:   fetcher,
		RepoPath:  "group/repo",
		Directory: "skills",
		Ref:       "main",
		Bundled:   b,
	})

	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v; want nil (graceful degradation)", err)
	}
	if len(got) != len(b) {
		t.Errorf("len = %d; want %d (bundled only)", len(got), len(b))
	}
	for _, s := range got {
		if !s.IsBundled() {
			t.Errorf("skill %q should be IsBundled() (remote failed)", s.Name)
		}
	}
	// The second Load call returns the same cached slice.
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("cached Load: %v", err)
	}
	if fetcher.listCalls > 1 {
		t.Errorf("listCalls = %d; remote should not be retried after cache hit", fetcher.listCalls)
	}
}

func TestLoad_RemoteFetchFailureForOneFile_KeepsBundledVersion(t *testing.T) {
	// Bundled has "go-review". Remote has "go-review" with a
	// fetch error. Per-file errors should not be fatal, and
	// since the remote "go-review" never made it into the
	// cache, the bundled version wins (because remote didn't
	// override it).
	b := bundled()
	b["go-review"] = []byte("# Bundled\n\nBundled body.")
	badPath := "skills/go-review.md"
	fetcher := &pathAwareFetcher{
		list: []gitlab.TreeNode{
			{ID: "1", Name: "go-review.md", Path: badPath, Type: "blob"},
		},
		files:  map[string][]byte{},
		failOn: map[string]error{badPath: errors.New("503 transient")},
	}
	l := New(Config{
		Fetcher:   fetcher,
		RepoPath:  "group/repo",
		Directory: "skills",
		Ref:       "main",
		Bundled:   b,
	})

	got, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var gr *Skill
	for i := range got {
		if got[i].Name == "go-review" {
			gr = &got[i]
			break
		}
	}
	if gr == nil {
		t.Fatal("go-review missing from result")
	}
	if !gr.IsBundled() {
		t.Errorf("go-review should still be bundled (remote fetch failed before override); got IsBundled=false")
	}
	if !strings.Contains(string(gr.Body), "Bundled body") {
		t.Errorf("body should be the bundled version; got %q", gr.Body)
	}
}

func TestSkill_IsBundled(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{bundledPathPrefix + "go-review.md", true},
		{"skills/go-review.md", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := Skill{Path: tc.path}.IsBundled()
			if got != tc.want {
				t.Errorf("Path=%q: IsBundled() = %v; want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestLoadBundled_FromFS(t *testing.T) {
	// Build a tiny in-memory FS and verify LoadBundled walks
	// it correctly. Catches regressions in the embedded-FS
	// loading path.
	fsys := fstest.MapFS{
		"go-review.md":        &fstest.MapFile{Data: []byte("body-1")},
		"testing-patterns.md": &fstest.MapFile{Data: []byte("body-2")},
		"README.txt":          &fstest.MapFile{Data: []byte("ignored")},
	}

	got, err := LoadBundled(fsys)
	if err != nil {
		t.Fatalf("LoadBundled: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d; want 2 (txt ignored)", len(got))
	}
	if string(got["go-review"]) != "body-1" {
		t.Errorf("go-review body = %q; want body-1", got["go-review"])
	}
	if string(got["testing-patterns"]) != "body-2" {
		t.Errorf("testing-patterns body = %q; want body-2", got["testing-patterns"])
	}
}

func TestLoadBundled_NilFS(t *testing.T) {
	got, err := LoadBundled(nil)
	if err != nil {
		t.Fatalf("LoadBundled(nil): %v", err)
	}
	if got == nil {
		t.Error("expected non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("len = %d; want 0", len(got))
	}
}

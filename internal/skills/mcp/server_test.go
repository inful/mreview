package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/skills"
)

// nopFetcher is a minimal SkillFetcher that returns empty results.
// Useful for tests that don't care about the transport — they
// pass a Loader configured with one or two canned skills via a
// pre-populated cache, then exercise the handler.
type nopFetcher struct{}

func (nopFetcher) ListRepositoryTree(context.Context, string, string, string) ([]gitlab.TreeNode, error) {
	return nil, nil
}

func (nopFetcher) GetRepositoryFileRaw(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}

// loadedLoader returns a Loader with a single Load call already
// applied. Tests use it to skip the GitLab discovery phase when
// only the MCP handler logic is in scope.
func loadedLoader(t *testing.T, repoPath, directory, ref string, canned []skills.Skill) *skills.Loader {
	t.Helper()
	// Use a listFetcher that always returns canned nodes and
	// files, so Load succeeds and populates the cache.
	fetcher := &listFetcher{
		list:   toTreeNodes(canned),
		bodies: skillBodies(canned),
	}
	l := skills.New(skills.Config{
		Fetcher:   fetcher,
		RepoPath:  repoPath,
		Directory: directory,
		Ref:       ref,
	})
	if _, err := l.Load(context.Background()); err != nil {
		t.Fatalf("seed Load: %v", err)
	}
	return l
}

// listFetcher serves a fixed list + bodies map. Lets tests
// pre-populate the Loader without mocking the transport per call.
type listFetcher struct {
	list   []gitlab.TreeNode
	bodies map[string][]byte
}

func (f *listFetcher) ListRepositoryTree(context.Context, string, string, string) ([]gitlab.TreeNode, error) {
	return f.list, nil
}

func (f *listFetcher) GetRepositoryFileRaw(_ context.Context, _, p, _ string) ([]byte, error) {
	return f.bodies[p], nil
}

func toTreeNodes(ss []skills.Skill) []gitlab.TreeNode {
	out := make([]gitlab.TreeNode, 0, len(ss))
	for _, s := range ss {
		out = append(out, gitlab.TreeNode{
			ID:   s.SHA,
			Name: s.Name + ".md",
			Path: s.Path,
			Type: "blob",
		})
	}
	return out
}

func skillBodies(ss []skills.Skill) map[string][]byte {
	out := make(map[string][]byte, len(ss))
	for _, s := range ss {
		out[s.Path] = s.Body
	}
	return out
}

// TestHandleListSkills_ReturnsCachedSkills verifies that the
// list_skills handler returns every cached skill with name,
// description, and path. Body bytes must NOT leak into the
// list_skills payload (that would defeat the point of
// list-then-read).
//
// The descriptions are derived by extractDescription from the
// canned bodies — the test asserts the round-trip behaves as
// expected, not that the canned Skill.Description field is
// preserved (Loader.Load overwrites that field on every Load).
func TestHandleListSkills_ReturnsCachedSkills(t *testing.T) {
	canned := []skills.Skill{
		{
			Name: "go-review", Path: "skills/go-review.md", SHA: "abc",
			Body: []byte("How to review Go code.\n\nDetailed body.\n"),
		},
		{
			Name: "tokensave-usage", Path: "skills/tokensave-usage.md", SHA: "def",
			Body: []byte("How to use tokensave.\n\nDetailed body.\n"),
		},
	}
	l := loadedLoader(t, "inful/mreview-skills", "skills", "main", canned)

	_, out, err := handleListSkills(context.Background(), l)
	if err != nil {
		t.Fatalf("handleListSkills: %v", err)
	}
	if out.Source != "inful/mreview-skills" {
		t.Errorf("source = %q; want inful/mreview-skills", out.Source)
	}
	if out.Count != 2 {
		t.Errorf("count = %d; want 2", out.Count)
	}
	if len(out.Skills) != 2 {
		t.Fatalf("skills len = %d; want 2", len(out.Skills))
	}

	// Build a name→summary lookup so the test is order-independent.
	byName := make(map[string]skillSummary, len(out.Skills))
	for _, s := range out.Skills {
		byName[s.Name] = s
	}
	for _, want := range canned {
		got, ok := byName[want.Name]
		if !ok {
			t.Errorf("missing skill %q in output", want.Name)
			continue
		}
		if got.Description == "" {
			t.Errorf("%s description is empty", want.Name)
		}
		if got.Path != want.Path {
			t.Errorf("%s path = %q; want %q", want.Name, got.Path, want.Path)
		}
	}

	// Body bytes must not leak into the list output. The
	// description field is bounded (200 chars), so the second
	// paragraph's text would only appear if extractDescription
	// grabbed it (it doesn't — extractDescription stops at the
	// first blank line).
	for _, s := range out.Skills {
		if strings.Contains(s.Description, "Detailed body.") {
			t.Errorf("description should not contain second paragraph: %q", s.Description)
		}
	}
}

// TestHandleListSkills_EmptyCacheReturnsEmptyArray verifies
// that an empty skill set returns [] (not nil) and count=0.
// The MCP agent relies on this shape to iterate safely.
func TestHandleListSkills_EmptyCacheReturnsEmptyArray(t *testing.T) {
	l := skills.New(skills.Config{
		Fetcher:   nopFetcher{},
		RepoPath:  "group/repo",
		Directory: "skills",
		Ref:       "main",
	})
	_, out, err := handleListSkills(context.Background(), l)
	if err != nil {
		t.Fatalf("handleListSkills: %v", err)
	}
	if out.Skills == nil {
		t.Error("Skills should be empty slice, not nil")
	}
	if len(out.Skills) != 0 {
		t.Errorf("len(Skills) = %d; want 0", len(out.Skills))
	}
	if out.Count != 0 {
		t.Errorf("Count = %d; want 0", out.Count)
	}
	if out.Source != "group/repo" {
		t.Errorf("Source = %q; want group/repo", out.Source)
	}
}

// TestHandleReadSkill_ReturnsBody verifies the happy path:
// read_skill returns the full body bytes for a known name.
func TestHandleReadSkill_ReturnsBody(t *testing.T) {
	canned := []skills.Skill{
		{Name: "go-review", Path: "skills/go-review.md", Description: "Go review guide.", Body: []byte("# Go Review\n\nDetailed guidance here.")},
	}
	l := loadedLoader(t, "group/repo", "skills", "main", canned)

	_, out, err := handleReadSkill(context.Background(), l, readSkillInput{Name: "go-review"})
	if err != nil {
		t.Fatalf("handleReadSkill: %v", err)
	}
	if out.Name != "go-review" {
		t.Errorf("name = %q; want go-review", out.Name)
	}
	if out.Path != "skills/go-review.md" {
		t.Errorf("path = %q; want skills/go-review.md", out.Path)
	}
	if out.Body != "# Go Review\n\nDetailed guidance here." {
		t.Errorf("body = %q; want full body", out.Body)
	}
}

// TestHandleReadSkill_UnknownName_ReturnsError verifies that an
// unknown name surfaces as a wrapped ErrSkillNotFound. The agent
// should be able to branch on errors.Is(err, skills.ErrSkillNotFound).
func TestHandleReadSkill_UnknownName_ReturnsError(t *testing.T) {
	canned := []skills.Skill{
		{Name: "go-review", Path: "skills/go-review.md", Body: []byte("body")},
	}
	l := loadedLoader(t, "group/repo", "skills", "main", canned)

	_, _, err := handleReadSkill(context.Background(), l, readSkillInput{Name: "no-such-skill"})
	if err == nil {
		t.Fatal("expected error for unknown skill")
	}
	if !errors.Is(err, skills.ErrSkillNotFound) {
		t.Errorf("err = %v; want errors.Is(err, skills.ErrSkillNotFound)", err)
	}
}

// TestHandleReadSkill_EmptyName_ReturnsError verifies that the
// handler rejects empty input up-front. Don't let the missing
// field propagate as a generic "not found".
func TestHandleReadSkill_EmptyName_ReturnsError(t *testing.T) {
	l := skills.New(skills.Config{
		Fetcher:   nopFetcher{},
		RepoPath:  "group/repo",
		Directory: "skills",
		Ref:       "main",
	})

	_, _, err := handleReadSkill(context.Background(), l, readSkillInput{Name: ""})
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error should mention name is required; got %v", err)
	}
}

// TestHandleReadSkill_BodyBytesAreVerbatim verifies that the
// handler does not render markdown or transform the body in
// any way. The agent decides how to interpret it.
func TestHandleReadSkill_BodyBytesAreVerbatim(t *testing.T) {
	rawBody := "# Title\n\n```go\nfunc Foo() {}\n```\n\n- bullet\n- another\n"
	canned := []skills.Skill{
		{Name: "code-heavy", Path: "skills/code-heavy.md", Body: []byte(rawBody)},
	}
	l := loadedLoader(t, "group/repo", "skills", "main", canned)

	_, out, err := handleReadSkill(context.Background(), l, readSkillInput{Name: "code-heavy"})
	if err != nil {
		t.Fatalf("handleReadSkill: %v", err)
	}
	if out.Body != rawBody {
		t.Errorf("body mutated:\n got: %q\nwant: %q", out.Body, rawBody)
	}
}

package gitlab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// discussionStub is a self-contained test server that captures
// every request and serves the queued response. Used only by the
// PostDiscussion position-shape tests below.
type discussionStub struct {
	*httptest.Server
	captured struct {
		method  string
		path    string
		headers http.Header
		body    string
	}
}

func newDiscussionStub(t *testing.T, status int, body string) *discussionStub {
	t.Helper()
	s := &discussionStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.captured.method = r.Method
		s.captured.path = r.URL.Path
		s.captured.headers = r.Header.Clone()
		s.captured.body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

// positionFromBody unmarshals only the `position` field of the
// request body. The rest is irrelevant for these assertions.
func positionFromBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var parsed struct {
		Position map[string]any `json:"position"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("decode body: %v\n%s", err, body)
	}
	if parsed.Position == nil {
		t.Fatalf("request body has no position field\n%s", body)
	}
	return parsed.Position
}

// TestPostDiscussion_PositionShape_NewFile confirms that for a
// newly-added file, only NewPath + NewLine are sent. OldPath and
// OldLine are OMITTED (not zero/empty) because GitLab treats 0 as
// "anchor to old line 0" and returns 500.
func TestPostDiscussion_PositionShape_NewFile(t *testing.T) {
	stub := newDiscussionStub(t, http.StatusCreated,
		`{"id":"d1","individual_note":false,"notes":[]}`)
	c := newTestClient(t, stub.URL)

	_, err := c.PostDiscussion(context.Background(), "group/project", 42, DiffRefs{
		BaseSHA:  "1111111111111111111111111111111111111111",
		StartSHA: "2222222222222222222222222222222222222222",
		HeadSHA:  "3333333333333333333333333333333333333333",
	}, InlineComment{
		File:    "pkg/new_feature.go",
		NewLine: 42,
		Body:    "consider extracting this",
		// OldPath and OldLine are intentionally zero — the test
		// confirms they're omitted from the wire, not sent as "" / 0.
	})
	if err != nil {
		t.Fatalf("PostDiscussion: %v", err)
	}

	if stub.captured.path != "/api/v4/projects/group/project/merge_requests/42/discussions" {
		t.Errorf("path = %q", stub.captured.path)
	}
	pos := positionFromBody(t, stub.captured.body)

	if got := pos["new_path"]; got != "pkg/new_feature.go" {
		t.Errorf("new_path = %v", got)
	}
	if got, ok := pos["new_line"]; !ok || got != float64(42) {
		// JSON numbers decode to float64 in map[string]any
		t.Errorf("new_line = %v (present=%v)", got, ok)
	}
	if _, present := pos["old_path"]; present {
		t.Errorf("old_path must NOT be sent for new files; got %v", pos["old_path"])
	}
	if _, present := pos["old_line"]; present {
		t.Errorf("old_line must NOT be sent for new files; got %v", pos["old_line"])
	}
	if got := pos["position_type"]; got != "text" {
		t.Errorf("position_type = %v, want text", got)
	}
	// diff_refs are always sent — those are the SHAs GitLab needs to anchor against.
	if got := pos["base_sha"]; got != "1111111111111111111111111111111111111111" {
		t.Errorf("base_sha = %v", got)
	}
}

// TestPostDiscussion_PositionShape_ModifiedFile confirms the
// most common case: a line in a modified file. We send NewPath +
// NewLine and (because it's not renamed) no OldPath.
func TestPostDiscussion_PositionShape_ModifiedFile(t *testing.T) {
	stub := newDiscussionStub(t, http.StatusCreated, `{"id":"d2","notes":[]}`)
	c := newTestClient(t, stub.URL)

	_, err := c.PostDiscussion(context.Background(), "group/project", 42, DiffRefs{
		BaseSHA:  "1111111111111111111111111111111111111111",
		StartSHA: "2222222222222222222222222222222222222222",
		HeadSHA:  "3333333333333333333333333333333333333333",
	}, InlineComment{
		File:    "pkg/existing.go",
		NewLine: 104,
		Body:    "x",
	})
	if err != nil {
		t.Fatalf("PostDiscussion: %v", err)
	}

	pos := positionFromBody(t, stub.captured.body)
	if got := pos["new_path"]; got != "pkg/existing.go" {
		t.Errorf("new_path = %v", got)
	}
	if got, ok := pos["new_line"]; !ok || got != float64(104) {
		t.Errorf("new_line = %v (present=%v)", got, ok)
	}
	if _, present := pos["old_path"]; present {
		t.Errorf("old_path must NOT be sent for non-renamed modified files; got %v", pos["old_path"])
	}
	if _, present := pos["old_line"]; present {
		t.Errorf("old_line must NOT be sent for modified files; got %v", pos["old_line"])
	}
}

// TestPostDiscussion_PositionShape_RenamedFile confirms that
// when the file was renamed, both NewPath and OldPath are sent
// (different values) so the comment anchors to the rename
// boundary.
func TestPostDiscussion_PositionShape_RenamedFile(t *testing.T) {
	stub := newDiscussionStub(t, http.StatusCreated, `{"id":"d3","notes":[]}`)
	c := newTestClient(t, stub.URL)

	_, err := c.PostDiscussion(context.Background(), "group/project", 42, DiffRefs{
		BaseSHA:  "1111111111111111111111111111111111111111",
		StartSHA: "2222222222222222222222222222222222222222",
		HeadSHA:  "3333333333333333333333333333333333333333",
	}, InlineComment{
		File:    "pkg/v2/feature.go", // new path
		OldPath: "pkg/v1/feature.go", // old path (renamed)
		NewLine: 12,
		Body:    "x",
	})
	if err != nil {
		t.Fatalf("PostDiscussion: %v", err)
	}

	pos := positionFromBody(t, stub.captured.body)
	if got := pos["new_path"]; got != "pkg/v2/feature.go" {
		t.Errorf("new_path = %v", got)
	}
	if got := pos["old_path"]; got != "pkg/v1/feature.go" {
		t.Errorf("old_path = %v (must be the old name for rename anchoring)", got)
	}
	if got, ok := pos["new_line"]; !ok || got != float64(12) {
		t.Errorf("new_line = %v (present=%v)", got, ok)
	}
	if _, present := pos["old_line"]; present {
		t.Errorf("old_line must NOT be sent; got %v", pos["old_line"])
	}
}

// TestPostDiscussion_PositionShape_DeletedFile confirms that
// for a deleted file, only OldPath + OldLine are sent (no new
// side of the diff exists).
func TestPostDiscussion_PositionShape_DeletedFile(t *testing.T) {
	stub := newDiscussionStub(t, http.StatusCreated, `{"id":"d4","notes":[]}`)
	c := newTestClient(t, stub.URL)

	_, err := c.PostDiscussion(context.Background(), "group/project", 42, DiffRefs{
		BaseSHA:  "1111111111111111111111111111111111111111",
		StartSHA: "2222222222222222222222222222222222222222",
		HeadSHA:  "3333333333333333333333333333333333333333",
	}, InlineComment{
		Body: "x",
		// File is the old path (no new path exists).
		File:    "pkg/dead_code.go",
		OldPath: "pkg/dead_code.go",
		OldLine: 7,
	})
	if err != nil {
		t.Fatalf("PostDiscussion: %v", err)
	}

	pos := positionFromBody(t, stub.captured.body)
	if _, present := pos["new_path"]; present {
		t.Errorf("new_path must NOT be sent for deleted files; got %v", pos["new_path"])
	}
	if got := pos["old_path"]; got != "pkg/dead_code.go" {
		t.Errorf("old_path = %v", got)
	}
	if _, present := pos["new_line"]; present {
		t.Errorf("new_line must NOT be sent for deleted files; got %v", pos["new_line"])
	}
	if got, ok := pos["old_line"]; !ok || got != float64(7) {
		t.Errorf("old_line = %v (present=%v)", got, ok)
	}
}

// TestPostDiscussion_AuthTokenSent is a smoke test that the
// PRIVATE-TOKEN auth header is set on /discussions POSTs — the
// regression guard for the GitLab 401 on this endpoint.
func TestPostDiscussion_AuthTokenSent(t *testing.T) {
	stub := newDiscussionStub(t, http.StatusCreated, `{"id":"d5","notes":[]}`)
	c := newTestClient(t, stub.URL)

	_, err := c.PostDiscussion(context.Background(), "group/project", 42, DiffRefs{
		BaseSHA:  "1111111111111111111111111111111111111111",
		StartSHA: "2222222222222222222222222222222222222222",
		HeadSHA:  "3333333333333333333333333333333333333333",
	}, InlineComment{File: "x.go", NewLine: 1, Body: "x"})
	if err != nil {
		t.Fatalf("PostDiscussion: %v", err)
	}

	if got := stub.captured.headers.Get("Private-Token"); got != "test-token-abc" {
		t.Errorf("Private-Token = %q, want test-token-abc", got)
	}
	if !strings.HasSuffix(stub.captured.path, "/discussions") {
		t.Errorf("path = %q; want suffix /discussions", stub.captured.path)
	}
}

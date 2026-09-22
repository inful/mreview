package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// noteFixture is the upstream JSON shape for a successful POST
// /merge_requests/:iid/notes.
const noteFixture = `{
  "id": 901,
  "body": "Looks good — 1 nit on caching.",
  "author": {"id": 7, "username": "review-bot", "name": "Review Bot"},
  "system": false,
  "resolvable": false,
  "web_url": "https://gitlab.example.com/group/project/-/merge_requests/42#note_901"
}`

// discussionFixture is the upstream JSON shape for a successful POST
// /merge_requests/:iid/discussions. We embed the full Note objects
// inside since the upstream type carries them.
const discussionFixture = `{
  "id": "disc-1",
  "individual_note": false,
  "notes": [
    {
      "id": 902,
      "body": "**[warning]** JWT secret should be cached.",
      "author": {"id": 7, "username": "review-bot", "name": "Review Bot"},
      "system": false
    }
  ]
}`

// sampleRefs are the SHAs used to anchor inline comments. Same as
// what FetchMR returns for a typical MR.
var sampleRefs = DiffRefs{
	BaseSHA:  "1111111111111111111111111111111111111111",
	HeadSHA:  "2222222222222222222222222222222222222222",
	StartSHA: "3333333333333333333333333333333333333333",
}

func TestPostSummary_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusCreated, noteFixture)
	c := newTestClient(t, stub.URL)

	got, err := c.PostSummary(context.Background(), "group/project", 42, "LGTM with 1 nit")
	if err != nil {
		t.Fatalf("PostSummary: %v", err)
	}
	if got.ID != 901 {
		t.Errorf("ID = %d, want 901", got.ID)
	}
	if got.Body != "Looks good — 1 nit on caching." {
		t.Errorf("Body = %q", got.Body)
	}
	if got.Author.Username != "review-bot" {
		t.Errorf("Author.Username = %q", got.Author.Username)
	}

	// Verify request shape.
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	req := stub.requests[0]
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.Path != "/api/v4/projects/group/project/merge_requests/42/notes" {
		t.Errorf("path = %q", req.Path)
	}
	// The body should be a JSON object with body="LGTM with 1 nit".
	var sent map[string]any
	if err := json.Unmarshal([]byte(req.Body), &sent); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, req.Body)
	}
	if sent["body"] != "LGTM with 1 nit" {
		t.Errorf("body field = %v, want %q", sent["body"], "LGTM with 1 nit")
	}
}

func TestPostSummary_EmptyBody(t *testing.T) {
	c := newTestClient(t, "http://unused")
	for _, body := range []string{"", "   ", "\n\t"} {
		_, err := c.PostSummary(context.Background(), "group/project", 42, body)
		if err == nil {
			t.Errorf("expected error for empty body %q", body)
		}
	}
}

func TestPostSummary_AuthError(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusUnauthorized, `{"message":"401 Unauthorized"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.PostSummary(context.Background(), "group/project", 42, "body")
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindAuth {
		t.Errorf("expected KindAuth, got %v", err)
	}
	if len(stub.requests) != 1 {
		t.Errorf("expected 1 request (no retry), got %d", len(stub.requests))
	}
}

func TestPostSummary_ProjectLocked(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusConflict, `{"message":"Project is locked"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.PostSummary(context.Background(), "group/project", 42, "body")
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindConflict {
		t.Errorf("expected KindConflict, got %v", err)
	}
}

func TestPostSummary_InvalidArgs(t *testing.T) {
	c := newTestClient(t, "http://unused")
	if _, err := c.PostSummary(context.Background(), "", 42, "body"); err == nil {
		t.Error("expected error for empty project")
	}
	if _, err := c.PostSummary(context.Background(), "ok", 0, "body"); err == nil {
		t.Error("expected error for zero iid")
	}
}

func TestPostDiscussion_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusCreated, discussionFixture)
	c := newTestClient(t, stub.URL)

	cmt := InlineComment{
		File:    "internal/auth/jwt.go",
		NewLine: 142,
		Body:    "**[warning]** JWT secret should be cached.",
	}
	got, err := c.PostDiscussion(context.Background(), "group/project", 42, sampleRefs, cmt)
	if err != nil {
		t.Fatalf("PostDiscussion: %v", err)
	}
	if got.ID != "disc-1" {
		t.Errorf("ID = %q, want disc-1", got.ID)
	}
	if len(got.Notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(got.Notes))
	}
	if got.Notes[0].Author.Username != "review-bot" {
		t.Errorf("note author = %q", got.Notes[0].Author.Username)
	}

	// Verify request shape: the upstream POST uses url-encoded
	// form fields with nested position[...]. We accept either
	// JSON-encoded or url-encoded bodies; both round-trip through
	// the upstream client. Check that position is present and
	// carries the right file/line + sha anchors.
	req := stub.requests[0]
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.Path != "/api/v4/projects/group/project/merge_requests/42/discussions" {
		t.Errorf("path = %q", req.Path)
	}
	// Body should mention the SHAs, the file, and the line.
	// The exact wire format depends on the upstream encoder; we
	// check for substring presence.
	required := []string{
		"internal/auth/jwt.go",
		sampleRefs.BaseSHA,
		sampleRefs.HeadSHA,
		sampleRefs.StartSHA,
		"142",
		"warning",
	}
	for _, want := range required {
		if !strings.Contains(req.Body, want) {
			t.Errorf("request body missing %q\n%s", want, req.Body)
		}
	}
}

func TestPostDiscussion_WithSuggestion(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusCreated, discussionFixture)
	c := newTestClient(t, stub.URL)

	cmt := InlineComment{
		File:       "a.go",
		NewLine:    10,
		Body:       "**[info]** refactor",
		Suggestion: "var x = 1",
	}
	_, err := c.PostDiscussion(context.Background(), "group/project", 42, sampleRefs, cmt)
	if err != nil {
		t.Fatalf("PostDiscussion: %v", err)
	}
	body := stub.requests[0].Body
	if !strings.Contains(body, "```suggestion") {
		t.Errorf("expected suggestion block, got %q", body)
	}
	if !strings.Contains(body, "var x = 1") {
		t.Errorf("expected suggestion text in body, got %q", body)
	}
}

func TestPostDiscussion_OldLineOnly(t *testing.T) {
	// Commenting on a deleted file: only OldLine + OldPath, NewLine=0.
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusCreated, discussionFixture)
	c := newTestClient(t, stub.URL)

	cmt := InlineComment{
		File:    "deleted.go",
		OldPath: "deleted.go",
		OldLine: 5,
		Body:    "**[warning]** removed but referenced elsewhere",
	}
	_, err := c.PostDiscussion(context.Background(), "group/project", 42, sampleRefs, cmt)
	if err != nil {
		t.Fatalf("PostDiscussion: %v", err)
	}
}

func TestPostDiscussion_LineOutOfRange_ClassifiedAsConflict(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusBadRequest, `{"message":"new_line is not in range"}`)
	c := newTestClient(t, stub.URL)

	cmt := InlineComment{File: "a.go", NewLine: 9999, Body: "x"}
	_, err := c.PostDiscussion(context.Background(), "group/project", 42, sampleRefs, cmt)
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindConflict {
		t.Errorf("expected KindConflict for line-out-of-range, got %v", err)
	}
}

func TestPostDiscussion_GenericBadRequest(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusBadRequest, `{"message":"malformed payload"}`)
	c := newTestClient(t, stub.URL)

	cmt := InlineComment{File: "a.go", NewLine: 10, Body: "x"}
	_, err := c.PostDiscussion(context.Background(), "group/project", 42, sampleRefs, cmt)
	if err == nil {
		t.Fatal("expected error")
	}
	// 400 without the line-range signal stays as KindBadRequest.
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindBadRequest {
		t.Errorf("expected KindBadRequest, got %v", err)
	}
}

func TestPostDiscussion_InvalidArgs(t *testing.T) {
	c := newTestClient(t, "http://unused")
	good := InlineComment{File: "a.go", NewLine: 10, Body: "x"}

	cases := []struct {
		name string
		run  func() error
	}{
		{"empty file", func() error {
			_, err := c.PostDiscussion(context.Background(), "p", 1, sampleRefs, InlineComment{NewLine: 1, Body: "x"})
			return err
		}},
		{"empty body", func() error {
			_, err := c.PostDiscussion(context.Background(), "p", 1, sampleRefs, InlineComment{File: "a", NewLine: 1})
			return err
		}},
		{"no line", func() error {
			_, err := c.PostDiscussion(context.Background(), "p", 1, sampleRefs, InlineComment{File: "a", Body: "x"})
			return err
		}},
		{"zero iid", func() error {
			_, err := c.PostDiscussion(context.Background(), "p", 0, sampleRefs, good)
			return err
		}},
		{"missing SHAs", func() error {
			_, err := c.PostDiscussion(context.Background(), "p", 1, DiffRefs{}, good)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestLooksLikeSHA(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"0123456789abcdef0123456789abcdef01234567", true},
		{"0123456789ABCDEF0123456789ABCDEF01234567", true},
		{"0123456789abcdef0123456789abcdef0123456789", false}, // 42 chars
		{"0123456789abcdef0123456789abcdef0123456", false},    // 39 chars
		{"0123456789abcdef0123456789abcdef0123456g", false},   // non-hex
		{"", false},
	}
	for _, tc := range cases {
		if got := looksLikeSHA(tc.s); got != tc.want {
			t.Errorf("looksLikeSHA(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestIsLineRangeError(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		// JSON form (what we get if the upstream hasn't pre-parsed).
		{`{"message":"new_line is not in range"}`, true},
		{`{"message":"old_line is not a valid line"}`, true},
		{`{"message":"position is not valid"}`, true},
		{`{"message":"malformed payload"}`, false},
		// Upstream's parseError() format (map printing).
		{`{message: new_line is not in range}`, true},
		{`{message: malformed payload}`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := isLineRangeError(tc.body); got != tc.want {
			t.Errorf("isLineRangeError(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// TestProjectNote_NilAuthor verifies the Author field is left empty
// when upstream returns an empty (zero-value) NoteAuthor. We can
// detect upstream's "no author" via the empty Username.
func TestProjectNote_EmptyAuthor(t *testing.T) {
	n := projectNote(nil) // nil note
	if n == nil {
		t.Fatal("projectNote(nil) returned nil; expected zero-value Note")
	}
	if n.Author.Username != "" {
		t.Errorf("nil note author should be empty, got %q", n.Author.Username)
	}
}

// silence unused-import lint for io (used by other test files in the
// package).
var _ = io.Discard

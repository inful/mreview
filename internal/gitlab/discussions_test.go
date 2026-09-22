package gitlab

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

const listDiscussionsFixture = `[
  {
    "id": "d1",
    "individual_note": false,
    "notes": [
      {"id": 1, "body": "**[warning]** first", "author": {"id": 1, "username": "review-bot", "name": "Bot"}, "system": false},
      {"id": 2, "body": "I agree", "author": {"id": 2, "username": "alice", "name": "Alice"}, "system": false}
    ]
  },
  {
    "id": "d2",
    "individual_note": true,
    "notes": [
      {"id": 3, "body": "summary note here", "author": {"id": 1, "username": "review-bot", "name": "Bot"}, "system": false}
    ]
  }
]`

func TestListDiscussions_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusOK, listDiscussionsFixture)
	c := newTestClient(t, stub.URL)

	discs, err := c.ListDiscussions(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ListDiscussions: %v", err)
	}
	if len(discs) != 2 {
		t.Fatalf("expected 2 discussions, got %d", len(discs))
	}
	if discs[0].ID != "d1" {
		t.Errorf("discs[0].ID = %q", discs[0].ID)
	}
	if len(discs[0].Notes) != 2 {
		t.Errorf("discs[0] should have 2 notes, got %d", len(discs[0].Notes))
	}

	// Verify the request shape.
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	if stub.requests[0].Path != "/api/v4/projects/group/project/merge_requests/42/discussions" {
		t.Errorf("path = %q", stub.requests[0].Path)
	}
}

func TestListDiscussions_AuthError(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusUnauthorized, `{"message":"401 Unauthorized"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.ListDiscussions(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindAuth {
		t.Errorf("expected KindAuth, got %v", err)
	}
}

func TestListDiscussions_InvalidArgs(t *testing.T) {
	c := newTestClient(t, "http://unused")
	if _, err := c.ListDiscussions(context.Background(), "", 42); err == nil {
		t.Error("expected error for empty project")
	}
	if _, err := c.ListDiscussions(context.Background(), "ok", 0); err == nil {
		t.Error("expected error for zero iid")
	}
}

func TestListDiscussions_TransientThenSuccess(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusBadGateway, `{"message":"upstream"}`)
	stub.enqueue(http.StatusOK, listDiscussionsFixture)
	c := newTestClient(t, stub.URL)

	discs, err := c.ListDiscussions(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ListDiscussions: %v", err)
	}
	if len(discs) != 2 {
		t.Errorf("expected 2 discussions, got %d", len(discs))
	}
}

func TestDiscussion_NotesBodies(t *testing.T) {
	d := Discussion{Notes: []Note{
		{Body: "first"},
		{Body: ""},
		{Body: "second"},
	}}
	got := d.NotesBodies()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("NotesBodies = %v", got)
	}
}

func TestDiscussion_FirstNoteBody(t *testing.T) {
	if got := (Discussion{}).FirstNoteBody(); got != "" {
		t.Errorf("empty discussion FirstNoteBody = %q", got)
	}
	d := Discussion{Notes: []Note{{Body: "alpha"}, {Body: "beta"}}}
	if got := d.FirstNoteBody(); got != "alpha" {
		t.Errorf("FirstNoteBody = %q, want alpha", got)
	}
}

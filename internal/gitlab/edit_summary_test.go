package gitlab

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const editSummaryFixture = `{"id":99,"body":"edited"}`

func TestEditSummary_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusOK, editSummaryFixture)
	c := newTestClient(t, stub.URL)

	body := "## edited body"
	note, err := c.EditSummary(context.Background(), "group/project", 42, 99, body)
	if err != nil {
		t.Fatalf("EditSummary: %v", err)
	}
	if note.ID != 99 {
		t.Errorf("note.ID = %d, want 99", note.ID)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	req := stub.requests[0]
	if req.Method != http.MethodPut {
		t.Errorf("method = %q, want PUT", req.Method)
	}
	wantPath := "/api/v4/projects/group/project/merge_requests/42/notes/99"
	if req.Path != wantPath {
		t.Errorf("path = %q, want %q", req.Path, wantPath)
	}
	// The body should be JSON with the new content.
	if !strings.Contains(req.Body, body) {
		t.Errorf("body should contain %q; got %q", body, req.Body)
	}
	_ = note
}

func TestEditSummary_NotFound(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusNotFound, `{"message":"404 Not Found"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.EditSummary(context.Background(), "group/project", 42, 99999, "body")
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindNotFound {
		t.Errorf("expected KindNotFound, got %v", err)
	}
}

func TestEditSummary_InvalidArgs(t *testing.T) {
	c := newTestClient(t, "http://unused")
	cases := []struct {
		name string
		fn   func() error
	}{
		{"empty project", func() error {
			_, err := c.EditSummary(context.Background(), "", 1, 1, "body")
			return err
		}},
		{"zero iid", func() error {
			_, err := c.EditSummary(context.Background(), "ok", 0, 1, "body")
			return err
		}},
		{"zero note id", func() error {
			_, err := c.EditSummary(context.Background(), "ok", 1, 0, "body")
			return err
		}},
		{"empty body", func() error {
			_, err := c.EditSummary(context.Background(), "ok", 1, 1, "")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil {
				t.Error("expected error")
			}
		})
	}
}

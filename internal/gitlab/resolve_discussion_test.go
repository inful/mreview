package gitlab

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const resolveFixture = `{"id":"d42","individual_note":false,"notes":[{"id":99,"body":"x"}]}`

func TestResolveDiscussion_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusOK, resolveFixture)
	c := newTestClient(t, stub.URL)

	if err := c.ResolveDiscussion(context.Background(), "group/project", 42, "d42"); err != nil {
		t.Fatalf("ResolveDiscussion: %v", err)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	req := stub.requests[0]
	// The URL is /discussions/:id (PUT); GitLab API requires
	// the body to carry `resolved=true`. Both must be right.
	if req.Method != http.MethodPut {
		t.Errorf("method = %q, want PUT", req.Method)
	}
	wantPath := "/api/v4/projects/group/project/merge_requests/42/discussions/d42"
	if req.Path != wantPath {
		t.Errorf("path = %q, want %q", req.Path, wantPath)
	}
	// The upstream client-go serialises struct options as JSON;
	// either form-encoded or JSON-encoded satisfies GitLab.
	// We pin on the JSON form since that's what we observed
	// in practice (and it round-trips with the *bool pointer
	// we pass in). The important property is that "resolved"
	// appears with a true value — exactly what the GitLab API
	// will toggle on.
	if !strings.Contains(req.Body, "\"resolved\":true") {
		t.Errorf("body should set resolved=true (JSON); got: %q", req.Body)
	}
}

func TestResolveDiscussion_NotFound(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusNotFound, `{"message":"404 Not Found"}`)
	c := newTestClient(t, stub.URL)

	err := c.ResolveDiscussion(context.Background(), "group/project", 42, "missing")
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindNotFound {
		t.Errorf("expected KindNotFound, got %v", err)
	}
}

func TestResolveDiscussion_InvalidArgs(t *testing.T) {
	c := newTestClient(t, "http://unused")
	cases := []struct {
		name string
		fn   func() error
	}{
		{"empty project", func() error { return c.ResolveDiscussion(context.Background(), "", 1, "d1") }},
		{"zero iid", func() error { return c.ResolveDiscussion(context.Background(), "ok", 0, "d1") }},
		{"empty discussion id", func() error { return c.ResolveDiscussion(context.Background(), "ok", 1, "") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil {
				t.Error("expected error")
			}
		})
	}
}

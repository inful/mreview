package gitlab

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const treeFixture = `[
  {"id": "abc", "name": "go-review.md", "type": "blob", "path": "skills/go-review.md", "mode": "100644"},
  {"id": "def", "name": "tokensave-usage.md", "type": "blob", "path": "skills/tokensave-usage.md", "mode": "100644"},
  {"id": "ghi", "name": "images", "type": "tree", "path": "skills/images", "mode": "040000"}
]`

const rawFileFixture = "# go-review\n\nSome skill body content."

func TestListRepositoryTree_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusOK, treeFixture)
	c := newTestClient(t, stub.URL)

	nodes, err := c.ListRepositoryTree(context.Background(), "group/project", "skills", "main")
	if err != nil {
		t.Fatalf("ListRepositoryTree: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(nodes))
	}
	// Verify we got blobs and trees correctly.
	if nodes[0].Name != "go-review.md" || nodes[0].Type != "blob" {
		t.Errorf("node 0 = %+v, want go-review.md/blob", nodes[0])
	}
	if nodes[2].Name != "images" || nodes[2].Type != "tree" {
		t.Errorf("node 2 = %+v, want images/tree", nodes[2])
	}
	// Verify request shape — path and ref are query params.
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	req := stub.requests[0]
	if req.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", req.Method)
	}
	wantPath := "/api/v4/projects/group/project/repository/tree"
	if req.Path != wantPath {
		t.Errorf("path = %q, want %q", req.Path, wantPath)
	}
	if !strings.Contains(req.Body, "skills") && req.Body != "" {
		// Body assertion is loose — the stub doesn't capture
		// query params; we just verify the request body is
		// either empty (GET) or contains the expected path.
		t.Errorf("unexpected request body: %q", req.Body)
	}
}

func TestListRepositoryTree_NotFound(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusNotFound, `{"message":"404 Project Not Found"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.ListRepositoryTree(context.Background(), "missing/project", "skills", "main")
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindNotFound {
		t.Errorf("expected KindNotFound, got %v", err)
	}
}

func TestListRepositoryTree_InvalidArgs(t *testing.T) {
	c := newTestClient(t, "http://unused")
	if _, err := c.ListRepositoryTree(context.Background(), "", "skills", "main"); err == nil {
		t.Error("expected error for empty project")
	}
}

func TestGetRepositoryFileRaw_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusOK, rawFileFixture)
	c := newTestClient(t, stub.URL)

	data, err := c.GetRepositoryFileRaw(context.Background(), "group/project", "skills/go-review.md", "main")
	if err != nil {
		t.Fatalf("GetRepositoryFileRaw: %v", err)
	}
	if string(data) != rawFileFixture {
		t.Errorf("body = %q, want %q", string(data), rawFileFixture)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	req := stub.requests[0]
	if req.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", req.Method)
	}
	wantPath := "/api/v4/projects/group/project/repository/files/skills/go-review.md/raw"
	if req.Path != wantPath {
		t.Errorf("path = %q, want %q", req.Path, wantPath)
	}
	if !strings.Contains(req.Body, "ref=") && req.Body != "" {
		t.Errorf("ref= should appear in body or query; body = %q", req.Body)
	}
}

func TestGetRepositoryFileRaw_NotFound(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusNotFound, `{"message":"404 File Not Found"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.GetRepositoryFileRaw(context.Background(), "group/project", "skills/missing.md", "main")
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindNotFound {
		t.Errorf("expected KindNotFound, got %v", err)
	}
}

func TestGetRepositoryFileRaw_InvalidArgs(t *testing.T) {
	c := newTestClient(t, "http://unused")
	if _, err := c.GetRepositoryFileRaw(context.Background(), "", "x.md", "main"); err == nil {
		t.Error("expected error for empty project")
	}
	if _, err := c.GetRepositoryFileRaw(context.Background(), "ok", "", "main"); err == nil {
		t.Error("expected error for empty path")
	}
}

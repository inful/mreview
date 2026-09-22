package gitlab

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

const userFixture = `{
  "id": 7,
  "username": "alice",
  "name": "Alice Example",
  "email": "alice@example.com"
}`

func TestCurrentUser_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusOK, userFixture)
	c := newTestClient(t, stub.URL)

	user, err := c.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if user.Username != "alice" {
		t.Errorf("Username = %q", user.Username)
	}
	if user.Name != "Alice Example" {
		t.Errorf("Name = %q", user.Name)
	}

	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	if stub.requests[0].Path != "/api/v4/user" {
		t.Errorf("path = %q, want /api/v4/user", stub.requests[0].Path)
	}
	if stub.requests[0].Method != http.MethodGet {
		t.Errorf("method = %s, want GET", stub.requests[0].Method)
	}
}

func TestCurrentUser_AuthError(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusUnauthorized, `{"message":"401 Unauthorized"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.CurrentUser(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindAuth {
		t.Errorf("expected KindAuth, got %v", err)
	}
}

func TestCurrentUser_ForbiddenError(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusForbidden, `{"message":"403 Forbidden"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.CurrentUser(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindAuth {
		t.Errorf("expected KindAuth for 403, got %v", err)
	}
}

func TestCurrentUser_TransientThenSuccess(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusServiceUnavailable, `{"message":"try again"}`)
	stub.enqueue(http.StatusOK, userFixture)
	c := newTestClient(t, stub.URL)

	user, err := c.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if user.Username != "alice" {
		t.Errorf("Username = %q", user.Username)
	}
	if len(stub.requests) != 2 {
		t.Errorf("expected 2 requests (retry), got %d", len(stub.requests))
	}
}

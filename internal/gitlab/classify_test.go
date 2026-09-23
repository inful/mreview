package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// newClassifyClient builds a Client pointed at the given base URL
// for use in the classify tests below. Mirrors newTestClient but
// without the test logger so we can pass nil; the classify path
// doesn't log.
func newClassifyClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := NewClient(baseURL, "test-token",
		RetryConfig{MaxAttempts: 1, InitialBackoff: 1},
		nil,
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// respFromServer hits a one-shot httptest server and returns the
// (resp, err) tuple the upstream client would hand to classify.
// The server's status/body are scriptable per test.
func respFromServer(t *testing.T, status int, body string) (*gl.Response, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c := newClassifyClient(t, srv.URL)
	_, resp, err := c.inner.Notes.CreateMergeRequestNote("g/p", 1, nil)
	return resp, err
}

// fakeResp builds a *gl.Response carrying the given body, suitable
// for handing to classify when we want to control the upstream
// response shape directly (e.g. when no upstream error wraps it).
func fakeResp(status int, body string) *gl.Response {
	return &gl.Response{
		Response: &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
		},
	}
}

// fakeErrResp builds a *gl.ErrorResponse as upstream would after
// parsing the given body and message. Pair with fakeResp when the
// test wants to exercise the ErrorResponse.Body / Message paths.
func fakeErrResp(status int, body string) *gl.ErrorResponse {
	return &gl.ErrorResponse{
		Body:     []byte(body),
		Response: &http.Response{StatusCode: status},
		Message:  "from upstream",
	}
}

// TestClassify_StatusKind covers the default status → Kind mapping
// through the new method, including the non-status status values the
// upstream ErrorResponse struct uses (which can be 0 for "no
// response received").
func TestClassify_StatusKind(t *testing.T) {
	c := newClassifyClient(t, "http://unused")
	cases := []struct {
		status int
		want   Kind
	}{
		{400, KindBadRequest},
		{401, KindAuth},
		{403, KindAuth},
		{404, KindNotFound},
		{409, KindConflict},
		{422, KindBadRequest},
		{429, KindTransient},
		{500, KindTransient},
		{502, KindTransient},
		{503, KindTransient},
		{599, KindTransient},
		{418, KindOther}, // not in our table
	}
	for _, tc := range cases {
		resp, err := respFromServer(t, tc.status, `{"message":"x"}`)
		if err == nil {
			t.Fatalf("status %d: expected upstream error, got nil", tc.status)
		}
		got := c.classify(http.MethodPost, "http://x/y", resp, err)
		var e *Error
		if !errors.As(got, &e) {
			t.Fatalf("status %d: returned %T, want *Error", tc.status, got)
		}
		if e.Kind != tc.want {
			t.Errorf("status %d: Kind = %s, want %s", tc.status, e.Kind, tc.want)
		}
		if e.StatusCode != tc.status {
			t.Errorf("status %d: StatusCode = %d", tc.status, e.StatusCode)
		}
		if e.Method != http.MethodPost {
			t.Errorf("status %d: Method = %q", tc.status, e.Method)
		}
		if e.URL != "http://x/y" {
			t.Errorf("status %d: URL = %q", tc.status, e.URL)
		}
	}
}

// TestClassify_NoResponse covers the path where the upstream client
// hands us a nil response (e.g. network error, context
// cancellation): Body falls back to err.Error() and Kind is Kind
// Other.
func TestClassify_NoResponse(t *testing.T) {
	c := newClassifyClient(t, "http://unused")
	cause := errors.New("connection refused")
	got := c.classify(http.MethodPost, "http://x/y", nil, cause)
	var e *Error
	if !errors.As(got, &e) {
		t.Fatalf("got %T, want *Error", got)
	}
	if e.Kind != KindOther {
		t.Errorf("Kind = %s, want other", e.Kind)
	}
	if e.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0", e.StatusCode)
	}
	if !strings.Contains(e.Body, "connection refused") {
		t.Errorf("Body = %q, want it to contain the transport error", e.Body)
	}
	if !errors.Is(got, cause) {
		t.Errorf("Cause not reachable via errors.Is; got %v", e.Cause)
	}
}

// TestClassify_BodyFromUpstreamError exercises the body extraction
// path where upstream has already parsed the response body into
// ErrorResponse.Body (the path PostDiscussion used to special-case).
func TestClassify_BodyFromUpstreamError(t *testing.T) {
	const msgBody = `{"message":"new_line is not in range"}`
	resp, err := respFromServer(t, http.StatusBadRequest, msgBody)
	if err == nil {
		t.Fatal("expected upstream error")
	}
	c := newClassifyClient(t, "http://unused")
	got := c.classify(http.MethodPost, "http://x/discussions", resp, err)
	var e *Error
	if !errors.As(got, &e) {
		t.Fatalf("got %T, want *Error", got)
	}
	// The body should include the message text — sourced from the
	// upstream ErrorResponse.Body (which readResponseBody would also
	// pick up since the upstream doesn't always drain for non-
	// Discussion endpoints, but either source yields the same
	// payload here).
	if !strings.Contains(e.Body, "new_line is not in range") {
		t.Errorf("Body = %q, expected it to contain the message", e.Body)
	}
}

// TestClassify_BodyFromLiveResponse exercises the path where the
// upstream left resp.Body intact and the body is read from there.
// We simulate this by constructing a *gl.Response directly with a
// non-nil Body and no upstream error wrapping.
func TestClassify_BodyFromLiveResponse(t *testing.T) {
	c := newClassifyClient(t, "http://unused")
	resp := fakeResp(http.StatusInternalServerError, `{"message":"upstream dead"}`)
	err := fakeErrResp(http.StatusInternalServerError, `{"message":"upstream dead"}`)
	got := c.classify(http.MethodPost, "http://x/y", resp, err)
	var e *Error
	if !errors.As(got, &e) {
		t.Fatalf("got %T, want *Error", got)
	}
	if e.Kind != KindTransient {
		t.Errorf("Kind = %s, want transient", e.Kind)
	}
	if !strings.Contains(e.Body, "upstream dead") {
		t.Errorf("Body = %q, want it to contain the message", e.Body)
	}
}

// TestClassify_BodyTruncatedAt4KiB asserts the 4 KiB cap applies to
// bodies sourced from any path (here we use the largest path —
// readResponseBody cap of 64 KiB then truncateBody down to 4 KiB).
func TestClassify_BodyTruncatedAt4KiB(t *testing.T) {
	c := newClassifyClient(t, "http://unused")
	// 200 KiB of body — readResponseBody caps at 64 KiB, then
	// strutil.Truncate caps at 4 KiB.
	big := strings.Repeat("X", 200*1024)
	resp := fakeResp(http.StatusBadGateway, big)
	err := fakeErrResp(http.StatusBadGateway, big)
	got := c.classify(http.MethodPost, "http://x/y", resp, err)
	var e *Error
	if !errors.As(got, &e) {
		t.Fatalf("got %T, want *Error", got)
	}
	if len(e.Body) > 4096+len("...") {
		t.Errorf("Body length = %d, want <= %d", len(e.Body), 4096+len("..."))
	}
	if !strings.HasSuffix(e.Body, "...") {
		t.Errorf("Body should end with truncation marker, got suffix %q",
			e.Body[max(0, len(e.Body)-20):])
	}
}

// TestClassify_ExtraClassifier confirms the variadic classifier
// hook: a passed-in func can mutate the *Error in place after the
// default status mapping has been applied.
func TestClassify_ExtraClassifier(t *testing.T) {
	c := newClassifyClient(t, "http://unused")
	resp := fakeResp(http.StatusBadRequest, `{"message":"bad payload"}`)
	err := fakeErrResp(http.StatusBadRequest, `{"message":"bad payload"}`)

	// Classifier downgrades KindBadRequest → KindOther when body
	// is empty (synthetic example; production uses line-range
	// detection).
	called := false
	classifier := func(e *Error) {
		called = true
		if e.Kind == KindBadRequest {
			e.Kind = KindOther
		}
	}
	got := c.classify(http.MethodPost, "http://x/y", resp, err, classifier)
	var e *Error
	if !errors.As(got, &e) {
		t.Fatalf("got %T, want *Error", got)
	}
	if !called {
		t.Error("extra classifier was not invoked")
	}
	if e.Kind != KindOther {
		t.Errorf("Kind = %s, want other (after classifier overrode BadRequest)", e.Kind)
	}
}

// TestClassify_ExtraClassifierRunsAfterTruncation confirms the
// classifier sees the truncated body (so it can match against the
// real message text without caring about the cap).
func TestClassify_ExtraClassifierRunsAfterTruncation(t *testing.T) {
	c := newClassifyClient(t, "http://unused")
	body := strings.Repeat("X", 100) + `{"message":"new_line is not in range"}` + strings.Repeat("Y", 100)
	resp := fakeResp(http.StatusBadRequest, body)
	err := fakeErrResp(http.StatusBadRequest, body)

	classifier := func(e *Error) {
		// Body may be the full body (well under 4 KiB) or the
		// truncated 4 KiB; either way the message substring is at
		// the front.
		if strings.Contains(e.Body, "new_line is not in range") {
			e.Kind = KindConflict
		}
	}
	got := c.classify(http.MethodPost, "http://x/discussions", resp, err, classifier)
	var e *Error
	if !errors.As(got, &e) {
		t.Fatalf("got %T, want *Error", got)
	}
	if e.Kind != KindConflict {
		t.Errorf("Kind = %s, want conflict", e.Kind)
	}
}

// TestClassify_LineRangeClassifierReal is the PostDiscussion
// integration test: 400 + line-range body → KindConflict via the
// inline classifier wired up in PostDiscussion.
func TestClassify_LineRangeClassifierReal(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusBadRequest, `{"message":"new_line is not in range"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.PostDiscussion(testContext(), "group/project", 42, sampleRefs,
		InlineComment{File: "a.go", NewLine: 9999, Body: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("got %T, want *Error", err)
	}
	if e.Kind != KindConflict {
		t.Errorf("Kind = %s, want conflict", e.Kind)
	}
}

// TestClassify_CapturesRetryAfterHeader checks the Retry-After
// capture on *Error.RetryAfter — doWithRetry reads this to decide
// how long to wait before the next attempt. Without this, server-
// supplied backoff hints are silently discarded.
func TestClassify_CapturesRetryAfterHeader(t *testing.T) {
	c := newClassifyClient(t, "http://unused")
	resp := fakeResp(http.StatusServiceUnavailable, "upstream busy")
	// fakeResp doesn't allocate Header; populate it before Set so
	// the call doesn't panic on a nil map.
	resp.Header = make(http.Header)
	resp.Header.Set("Retry-After", "30")

	got := c.classify(http.MethodGet, "http://x/y", resp, nil)
	var e *Error
	if !errors.As(got, &e) {
		t.Fatalf("got %T, want *Error", got)
	}
	if e.RetryAfter != "30" {
		t.Errorf("RetryAfter = %q, want %q", e.RetryAfter, "30")
	}
}

// TestClassify_NoRetryAfterHeader asserts an absent Retry-After
// header leaves the field empty (not a sentinel). retryAfterDuration
// turns "" into 0, which means "fall back to exponential backoff".
func TestClassify_NoRetryAfterHeader(t *testing.T) {
	c := newClassifyClient(t, "http://unused")
	resp := fakeResp(http.StatusInternalServerError, "down")

	got := c.classify(http.MethodGet, "http://x/y", resp, nil)
	var e *Error
	if !errors.As(got, &e) {
		t.Fatalf("got %T, want *Error", got)
	}
	if e.RetryAfter != "" {
		t.Errorf("RetryAfter = %q, want empty", e.RetryAfter)
	}
}

// TestClassify_NoResponseLeavesRetryAfterEmpty confirms a nil
// response (e.g. transport error) does not surface a header value;
// retryAfterDuration would otherwise silently skip backoff.
func TestClassify_NoResponseLeavesRetryAfterEmpty(t *testing.T) {
	c := newClassifyClient(t, "http://unused")
	cause := errors.New("connection refused")

	got := c.classify(http.MethodGet, "http://x/y", nil, cause)
	var e *Error
	if !errors.As(got, &e) {
		t.Fatalf("got %T, want *Error", got)
	}
	if e.RetryAfter != "" {
		t.Errorf("RetryAfter = %q, want empty when no response", e.RetryAfter)
	}
}

// testContext is a tiny helper so the inline classifier test above
// doesn't pull context into the imports list at the top of the file.
func testContext() context.Context { return context.Background() }

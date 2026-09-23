// Package gitlab wraps the official gitlab.com/gitlab-org/api/client-go
// client with two things the upstream client does not give us out of the
// box:
//
//   - Typed errors. Upstream returns *gitlab.ErrorResponse whose .StatusCode
//     we have to inspect at every call site. We map that into a small
//     enum (Auth / NotFound / Conflict / Transient / BadRequest / Other)
//     so the caller can switch on the kind without re-implementing the
//     status → category table.
//
//   - Retry with backoff + Retry-After honor. Upstream uses go-retryablehttp
//     internally, but its policy isn't surfaced in a way that lets us
//     configure per-call, log attempts, or surface a typed exhausted
//     error. doWithRetry wraps an arbitrary call with the policy described
//     by RetryConfig and returns either nil or *Error{Kind: Transient}.
//
// The package is intentionally narrow: it only exposes the calls the
// reviewer needs (FetchMR, FetchChanges, PostDiscussion, PostSummary).
// Anything else lives in upstream's client-go and isn't re-exported.
package gitlab

import (
	"errors"
	"fmt"
	"net/http"
)

// Kind classifies a GitLab API error so callers can switch on intent
// (auth / not-found / conflict / transient / etc.) without inspecting
// raw HTTP status codes.
type Kind int

const (
	// KindOther is the zero value; reserved for errors that aren't
	// mapped to a known category (typically an internal/programmer
	// error rather than a GitLab-side problem).
	KindOther Kind = iota
	// KindBadRequest is the kind for HTTP 400 / 422 — the request
	// was malformed or referenced an invalid resource. Not
	// retryable; fix the caller.
	KindBadRequest
	// KindAuth is the kind for HTTP 401 / 403 — token rejected or
	// insufficient scope. Maps to ExitAuth (3).
	KindAuth
	// KindNotFound is the kind for HTTP 404 — resource doesn't
	// exist. Maps to ExitNotFound (4).
	KindNotFound
	// KindConflict is the kind for HTTP 409 — request conflicts
	// with current state (stale MR head, lock contention). Maps to
	// ExitConflict (5).
	KindConflict
	// KindTransient is the kind for HTTP 408 / 429 / 5xx — server
	// asked us to back off or returned a transient error. Retryable;
	// maps to ExitTransient (6) after retries are exhausted.
	KindTransient
)

// String returns a stable label suitable for logging and metrics.
func (k Kind) String() string {
	switch k {
	case KindBadRequest:
		return "bad_request"
	case KindAuth:
		return "auth"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindTransient:
		return "transient"
	default:
		return "other"
	}
}

// ClassifyStatus maps an HTTP status code to its Kind.
//
// The mapping matches the GitLab API docs for the MR / discussion /
// note endpoints; status codes not listed here map to KindOther so the
// caller can decide how to handle them.
func ClassifyStatus(status int) Kind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return KindAuth
	case status == http.StatusNotFound:
		return KindNotFound
	case status == http.StatusConflict:
		return KindConflict
	case status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		(status >= 500 && status <= 599):
		return KindTransient
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return KindBadRequest
	default:
		return KindOther
	}
}

// Error is the typed error returned by every Client method.
//
// It is constructable directly so retry / classify helpers can wrap
// upstream errors uniformly. Callers should treat it as opaque except
// for Kind, StatusCode, Method, URL, Body, RetryAfter (all read-only
// fields).
type Error struct {
	Kind       Kind
	StatusCode int    // 0 when no HTTP response was received
	Method     string // "GET" / "POST" / etc.
	URL        string // full request URL
	Body       string // response body, truncated to 4 KiB
	Cause      error  // underlying error (e.g. context.Canceled); may be nil

	// RetryAfter carries the raw value of the upstream Retry-After
	// HTTP response header, when the response carried one and the
	// header was reachable. Empty when the header was absent, when
	// the upstream request never received a response, or when the
	// caller constructed *Error directly without populating it.
	//
	// doWithRetry parses this via parseRetryAfterHeader (delta-
	// seconds and HTTP-date forms both supported) and caps the
	// result at RetryConfig.RetryAfterCap. A header value the parser
	// cannot understand is treated as if absent — exponential
	// backoff takes over.
	RetryAfter string
}

// Error implements the error interface. Format:
//
//	"<method> <url>: <kind> (<status>): <body>"
//
// When StatusCode is 0 the parentheses are omitted; when Cause is
// non-nil it's appended after the body.
func (e *Error) Error() string {
	status := ""
	if e.StatusCode != 0 {
		status = fmt.Sprintf(" (%d)", e.StatusCode)
	}
	cause := ""
	if e.Cause != nil {
		cause = fmt.Sprintf(": %v", e.Cause)
	}
	return fmt.Sprintf("%s %s: %s%s: %s%s",
		e.Method, e.URL, e.Kind, status, e.Body, cause)
}

// Unwrap exposes the underlying cause for errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// Is supports matching by Kind in errors.Is chains.
//
//	if errors.Is(err, &gitlab.Error{Kind: gitlab.KindAuth}) { ... }
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return t.Kind == e.Kind
}

// AsError extracts a *Error from an error chain, or nil if the chain
// has none.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

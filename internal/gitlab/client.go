package gitlab

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/inful/mreview/internal/strutil"
)

// Client is a thin wrapper over the official gitlab.com/client-go
// client. It adds:
//   - a single NewClient entrypoint that owns base URL + token config
//   - typed errors via (*Client).classify
//   - retry via doWithRetry
//   - consistent slog context (repo, mr_iid) on every log line
//
// The wrapper is intentionally narrow: it exposes only the calls the
// reviewer needs (FetchMR, FetchChanges, PostDiscussion, PostNote).
// Anything else is reachable through the embedded inner client.
type Client struct {
	inner   *gl.Client
	baseURL string
	retry   RetryConfig
	logger  *slog.Logger
}

// NewClient builds a Client pointed at baseURL with the given token.
//
// baseURL may be either:
//   - the GitLab API root (e.g. "https://gitlab.com/api/v4"), the
//     shape the official client-go SDK expects, OR
//   - the GitLab web UI base URL (e.g. "https://gitlab.com" or
//     "$CI_SERVER_URL" from a CI pipeline). When the URL does
//     not already end with "/api/v4", NewClient appends it
//     automatically. This is the form the GitLab docs
//     recommend passing for CI integrations (the
//     predefined variable is $CI_SERVER_URL) and the form
//     that catches operators who don't realise the SDK
//     doesn't add the API path itself.
//
// The auto-prepend is idempotent: a URL that already ends with
// "/api/v4" is passed through unchanged. A trailing slash is
// tolerated ("https://gitlab.com/" → "https://gitlab.com/api/v4").
// When the auto-prepend fires, a debug line is logged so the
// operator can see what was used.
//
// token is a Personal Access Token with `api` scope. The token is
// sent as PRIVATE-TOKEN (the GitLab default for PATs); OAuth tokens
// would need a separate code path.
//
// If logger is nil, slog.Default() is used.
func NewClient(baseURL, token string, retry RetryConfig, logger *slog.Logger) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("gitlab: baseURL is required")
	}
	if token == "" {
		return nil, fmt.Errorf("gitlab: token is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Auto-prepend "/api/v4" when the operator passed the
	// web-UI base URL instead of the API root. This makes
	// the GitLab-CI pattern (--gitlab-url=$CI_SERVER_URL) work
	// out of the box; the SDK doesn't do this itself, and
	// without it the request URL becomes
	// "<base>/projects/.../merge_requests/..." which GitLab
	// returns 404 for (it expects "<base>/api/v4/...").
	apiURL := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(apiURL, "/api/v4") {
		apiURL += "/api/v4"
		logger.Debug("gitlab: auto-prepended /api/v4 to baseURL",
			"input", baseURL,
			"normalized", apiURL,
		)
	}

	inner, err := gl.NewClient(token,
		gl.WithBaseURL(apiURL),
		gl.WithoutRetries(), // we own the retry policy via doWithRetry
	)
	if err != nil {
		return nil, fmt.Errorf("gitlab: build client: %w", err)
	}
	return &Client{
		inner:   inner,
		baseURL: apiURL,
		retry:   retry,
		logger:  logger,
	}, nil
}

// Inner exposes the official client for callers that need access to
// endpoints the wrapper doesn't cover. Read-only access; mutating
// calls should go through wrapper methods so they get retry + logging.
func (c *Client) Inner() *gl.Client { return c.inner }

// classify turns an upstream API response + transport error into a
// typed *Error. It factors out the URL/status/body extraction pattern
// shared by every endpoint wrapper so individual call sites stay
// one-liners.
//
// Default mapping: status → Kind via ClassifyStatus. When no response
// was received (status == 0, err != nil) the returned error has
// Kind: KindOther and Body: err.Error(). When err is non-nil the
// upstream ErrorResponse (if any) is the preferred body source — the
// upstream client sometimes drains resp.Body during CheckResponse,
// and the parsed Body / Message fields are the only thing left.
//
// Callers can refine the result with extraClassifiers, which run in
// order after the default mapping and may mutate the *Error in place.
// PostDiscussion uses this to remap 400 → KindConflict when the body
// indicates a line-range error.
func (c *Client) classify(method, url string, resp *gl.Response, err error, extraClassifiers ...func(*Error)) error {
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}

	e := &Error{
		Kind:       KindOther,
		Method:     method,
		URL:        url,
		StatusCode: status,
		Cause:      err,
	}
	switch {
	case status != 0:
		// Got a response. Pick the best body source and the default
		// kind from the status code.
		e.Body = strutil.Truncate(bodyFromResponse(resp, err), 4096)
		e.Kind = ClassifyStatus(status)
		// Capture the Retry-After header verbatim; the retry layer
		// parses it via parseRetryAfterHeader. Done after the
		// status-classification switch so the Get() happens once.
		if resp != nil && resp.Response != nil {
			e.RetryAfter = resp.Header.Get("Retry-After")
		}
	case err != nil:
		// No response — surface the transport error as the body so
		// the caller has something concrete in logs.
		e.Body = strutil.Truncate(err.Error(), 4096)
	}
	// Body is already truncated inside both branches; the else
	// (status==0 && err==nil) path leaves Body empty.

	for _, ec := range extraClassifiers {
		ec(e)
	}
	return e
}

// bodyFromResponse picks the best available source for the response
// body, in priority order:
//
//  1. The upstream ErrorResponse's Body field — set when the upstream
//     client parsed the response body before returning. This is the
//     only source left after upstream drains resp.Body during
//     CheckResponse (the Discussions endpoint does this).
//  2. The upstream ErrorResponse's Message field — set when the
//     upstream client extracted just the message string.
//  3. The live resp.Body — what the upstream leaves behind when it
//     doesn't drain.
//
// Returns "" when none of the sources have content.
func bodyFromResponse(resp *gl.Response, err error) string {
	if errResp := asUpstreamError(err); errResp != nil {
		if len(errResp.Body) > 0 {
			return string(errResp.Body)
		}
		if errResp.Message != "" {
			return errResp.Message
		}
	}
	if resp != nil {
		return readResponseBody(resp.Body)
	}
	return ""
}

// asUpstreamError extracts a *gl.ErrorResponse from the upstream
// error chain. Returns nil when the error isn't an upstream one
// (e.g. context cancellation, network error).
func asUpstreamError(err error) *gl.ErrorResponse {
	if err == nil {
		return nil
	}
	var er *gl.ErrorResponse
	if errors.As(err, &er) {
		return er
	}
	return nil
}

// validatePath is a cheap sanity check on the project path argument
// (e.g. "group/project"). It rejects empty paths and paths with
// characters GitLab would never accept.
func validatePath(p string) error {
	if p == "" {
		return errors.New("gitlab: project path is empty")
	}
	if strings.ContainsAny(p, " \t\n\r") {
		return fmt.Errorf("gitlab: project path contains whitespace: %q", p)
	}
	return nil
}

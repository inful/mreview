package gitlab

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// Client is a thin wrapper over the official gitlab.com/client-go
// client. It adds:
//   - a single NewClient entrypoint that owns base URL + token config
//   - typed errors via classifyAndWrap
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
// baseURL must be the API root (e.g. "https://gitlab.com/api/v4" or
// "https://gitlab.example.com/api/v4"). The official client appends
// "/projects/:pid/merge_requests/:iid" etc. to it automatically.
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
	inner, err := gl.NewClient(token,
		gl.WithBaseURL(baseURL),
		gl.WithoutRetries(), // we own the retry policy via doWithRetry
	)
	if err != nil {
		return nil, fmt.Errorf("gitlab: build client: %w", err)
	}
	return &Client{
		inner:   inner,
		baseURL: baseURL,
		retry:   retry,
		logger:  logger,
	}, nil
}

// Inner exposes the official client for callers that need access to
// endpoints the wrapper doesn't cover. Read-only access; mutating
// calls should go through wrapper methods so they get retry + logging.
func (c *Client) Inner() *gl.Client { return c.inner }

// classifyAndWrap converts an upstream API response + body into a
// typed *Error. status==0 means no response was received (network
// error); in that case the returned error is *Error{Kind: Other} with
// Cause set.
func classifyAndWrap(method, url string, status int, body string, cause error) error {
	if status == 0 && cause != nil {
		return &Error{
			Kind:   KindOther,
			Method: method,
			URL:    url,
			Body:   cause.Error(),
			Cause:  cause,
		}
	}
	return &Error{
		Kind:       ClassifyStatus(status),
		StatusCode: status,
		Method:     method,
		URL:        url,
		Body:       truncateBody(body),
		Cause:      cause,
	}
}

// truncateBody caps the body stored in Error.Body at 4 KiB. Long
// error pages blow up logs and aren't useful for diagnosis.
func truncateBody(body string) string {
	const bodyMax = 4096
	if len(body) <= bodyMax {
		return body
	}
	return body[:bodyMax] + "...(truncated)"
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

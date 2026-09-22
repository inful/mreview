package gitlab

import (
	"context"
	"fmt"
	"net/http"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// CurrentUser returns the authenticated user (the token holder).
//
// This is the canonical "is my token valid?" check: every
// working GitLab token can hit /api/v4/user. The returned User
// also gives the reviewer the bot's username (useful for
// dedupe — see internal/reviewer/dedupe.go).
func (c *Client) CurrentUser(ctx context.Context) (*User, error) {
	var result *User
	op := "CurrentUser"
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		user, resp, err := c.inner.Users.CurrentUser()
		if err != nil {
			return classifyCurrentUserError(op, c.baseURL, resp, err)
		}
		result = &User{
			Username: user.Username,
			Name:     user.Name,
		}
		c.logger.Debug("gitlab health ok",
			"op", op,
			"attempt", attempt,
			"username", result.Username,
		)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// classifyCurrentUserError maps the upstream CurrentUser error to
// a typed *Error.
func classifyCurrentUserError(op, baseURL string, resp *gl.Response, err error) error {
	method := http.MethodGet
	url := fmt.Sprintf("%s/user", baseURL)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	respBody := ""
	if resp != nil {
		respBody = readResponseBody(resp.Body)
	}
	return classifyAndWrap(method, url, status, respBody, err)
}

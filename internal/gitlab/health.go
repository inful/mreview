package gitlab

import (
	"context"
	"fmt"
	"net/http"
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
	url := fmt.Sprintf("%s/user", c.baseURL)
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		user, resp, err := c.inner.Users.CurrentUser()
		if err != nil {
			return c.classify(http.MethodGet, url, resp, err)
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

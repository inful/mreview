package gitlab

import (
	"context"
	"fmt"
	"net/http"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// ResolveDiscussion marks an MR discussion thread as resolved.
//
// Resolved threads collapse to "Show resolved comments" by
// default in the GitLab UI, which is the visual signal we
// want when a new mreview run supersedes a prior one. We
// intentionally don't delete the prior thread — the operator
// may still want to dig into the history — we just bury it.
//
// Errors are typed via (*Client).classify; the caller can
// switch on Kind to distinguish auth failures from a 404
// (the discussion was already deleted out-of-band) from a
// 409 (the project is in a read-only state, e.g. a merge in
// progress). The caller may treat non-404 errors as soft
// failures — if a resolve call fails, the rest of the
// review still proceeds and posts a fresh summary.
func (c *Client) ResolveDiscussion(ctx context.Context, project string, iid int, discussionID string) error {
	if err := validatePath(project); err != nil {
		return err
	}
	if iid <= 0 {
		return fmt.Errorf("gitlab: merge request IID must be > 0, got %d", iid)
	}
	if discussionID == "" {
		return fmt.Errorf("gitlab: discussion ID is required")
	}

	resolved := true
	op := "ResolveDiscussion"
	url := fmt.Sprintf("%s/projects/%s/merge_requests/%d/discussions/%s",
		c.baseURL, project, iid, discussionID)
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		opts := &gl.ResolveMergeRequestDiscussionOptions{
			Resolved: &resolved,
		}
		_, resp, err := c.inner.Discussions.ResolveMergeRequestDiscussion(
			project, int64(iid), discussionID, opts)
		if err != nil {
			return c.classify(http.MethodPut, url, resp, err)
		}
		c.logger.Debug("gitlab resolve ok",
			"op", op,
			"project", project,
			"iid", iid,
			"discussion_id", discussionID,
			"attempt", attempt,
		)
		return nil
	})
	return err
}

package gitlab

import (
	"context"
	"fmt"
	"net/http"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// ListDiscussions returns every discussion thread on the MR.
//
// Pagination: upstream defaults to 20 per page; for a typical MR
// (≤20 discussions) one call is enough. For larger MRs the
// caller can iterate via the returned slice + opt.Page (future
// enhancement).
func (c *Client) ListDiscussions(ctx context.Context, project string, iid int) ([]Discussion, error) {
	if err := validatePath(project); err != nil {
		return nil, err
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlab: merge request IID must be > 0, got %d", iid)
	}

	var result []Discussion
	op := "ListDiscussions"
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		opts := &gl.ListMergeRequestDiscussionsOptions{
			ListOptions: gl.ListOptions{PerPage: 100},
		}
		discs, resp, err := c.inner.Discussions.ListMergeRequestDiscussions(project, int64(iid), opts)
		if err != nil {
			return classifyListDiscussionsError(op, c.baseURL, project, iid, resp, err)
		}
		result = make([]Discussion, 0, len(discs))
		for _, d := range discs {
			result = append(result, *projectDiscussion(d))
		}
		c.logger.Debug("gitlab fetch ok",
			"op", op,
			"project", project,
			"iid", iid,
			"attempt", attempt,
			"discussions", len(result),
		)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// classifyListDiscussionsError maps an upstream ListDiscussions
// error to a typed *Error.
func classifyListDiscussionsError(op, baseURL, project string, iid int, resp *gl.Response, err error) error {
	method := http.MethodGet
	url := fmt.Sprintf("%s/projects/%s/merge_requests/%d/discussions", baseURL, project, iid)
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

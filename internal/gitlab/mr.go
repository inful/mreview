package gitlab

import (
	"context"
	"fmt"
	"net/http"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// MergeRequest is the subset of *gitlab.MergeRequest that the
// reviewer actually consumes. The fields are a flat, JSON-marshalable
// projection so callers don't need to import the upstream package.
type MergeRequest struct {
	IID          int64  `json:"iid"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Author       User   `json:"author"`
	WebURL       string `json:"web_url"`
	DiffRefs     `json:"diff_refs"`
}

// User is a minimal projection of *gitlab.BasicUser. Email is omitted
// because the upstream BasicUser doesn't carry one — only the full
// *gitlab.User does, and we don't need the round-trip.
type User struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

// DiffRefs are the SHAs used to anchor inline discussion comments on
// the MR's source / target branches. Every inline comment must be
// posted with base_sha, start_sha, head_sha set to these values or
// GitLab returns 400.
type DiffRefs struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	StartSHA string `json:"start_sha"`
}

// FetchMR fetches a merge request by project path and IID.
//
// The project path is the GitLab URL slug (e.g. "group/project"),
// NOT the numeric project ID — the official client accepts both, but
// the slug is what every webhook payload carries.
//
// Errors are typed via (*Client).classify so callers can switch on
// Kind without parsing the HTTP status.
func (c *Client) FetchMR(ctx context.Context, project string, iid int) (*MergeRequest, error) {
	if err := validatePath(project); err != nil {
		return nil, err
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlab: merge request IID must be > 0, got %d", iid)
	}

	var result *MergeRequest
	op := "FetchMR"
	url := fmt.Sprintf("%s/projects/%s/merge_requests/%d", c.baseURL, project, iid)
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		mr, resp, err := c.inner.MergeRequests.GetMergeRequest(project, int64(iid), nil)
		if err != nil {
			return c.classify(http.MethodGet, url, resp, err)
		}
		result = projectMR(mr)
		c.logger.Debug("gitlab fetch ok",
			"op", op,
			"project", project,
			"iid", iid,
			"attempt", attempt,
			"title", result.Title,
		)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// projectMR projects an upstream *gitlab.MergeRequest onto our flat
// MergeRequest struct. Centralised so callers don't need to import
// the upstream package.
func projectMR(mr *gl.MergeRequest) *MergeRequest {
	out := &MergeRequest{
		IID:          mr.IID,
		Title:        mr.Title,
		Description:  mr.Description,
		State:        mr.State,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		WebURL:       mr.WebURL,
		DiffRefs: DiffRefs{
			BaseSHA:  mr.DiffRefs.BaseSha,
			HeadSHA:  mr.DiffRefs.HeadSha,
			StartSHA: mr.DiffRefs.StartSha,
		},
	}
	if mr.Author != nil {
		out.Author = User{
			Username: mr.Author.Username,
			Name:     mr.Author.Name,
		}
	}
	return out
}

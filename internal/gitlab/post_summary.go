package gitlab

import (
	"context"
	"fmt"
	"net/http"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// PostSummary posts a regular MR note (a top-level comment, not
// anchored to a file/line). This is what the reviewer uses to post
// the overall verdict / summary thread.
//
// Errors are typed via classifyAndWrap; the caller can switch on
// Kind to distinguish auth failures from project locks (409) etc.
//
// TODO: collapse into a single shared classifier (issue #3) — this
// classifyPostSummaryError currently duplicates the URL/status/body
// pattern used by classifyPostDiscussionError and the four
// classify*Error helpers in the read paths.
func (c *Client) PostSummary(ctx context.Context, project string, iid int, body string) (*Note, error) {
	if err := validatePost(project, iid, body); err != nil {
		return nil, err
	}
	var result *Note
	op := "PostSummary"
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		opts := &gl.CreateMergeRequestNoteOptions{
			Body: &body,
		}
		note, resp, err := c.inner.Notes.CreateMergeRequestNote(project, int64(iid), opts)
		if err != nil {
			return classifyPostSummaryError(op, c.baseURL, project, iid, resp, err)
		}
		result = projectNote(note)
		c.logger.Debug("gitlab post ok",
			"op", op,
			"project", project,
			"iid", iid,
			"attempt", attempt,
			"note_id", result.ID,
		)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// classifyPostSummaryError maps the upstream error from
// CreateMergeRequestNote into our typed *Error.
func classifyPostSummaryError(op, baseURL, project string, iid int, resp *gl.Response, err error) error {
	method := http.MethodPost
	url := fmt.Sprintf("%s/projects/%s/merge_requests/%d/notes", baseURL, project, iid)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	body := ""
	if resp != nil {
		body = readResponseBody(resp.Body)
	}
	return classifyAndWrap(method, url, status, body, err)
}

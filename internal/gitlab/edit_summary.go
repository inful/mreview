package gitlab

import (
	"context"
	"fmt"
	"net/http"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// EditSummary rewrites the body of a previously-posted MR
// note (typically the mreview summary post).
//
// The orchestrator uses this in the dedup flow: the
// summary is posted first with a commit-only marker
// (`<!-- mreview:commit=X -->`); after the inline findings
// post, their discussion IDs are known, so we edit the
// summary to include the full marker (`<!-- mreview:commit=X
// findings=ID1,ID2 -->`). One edit, no second summary
// post, no edit on GitLab's "Show all notes" timeline.
//
// Errors are typed via (*Client).classify.
func (c *Client) EditSummary(ctx context.Context, project string, iid int, noteID int64, body string) (*Note, error) {
	if err := validatePath(project); err != nil {
		return nil, err
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlab: merge request IID must be > 0, got %d", iid)
	}
	if noteID <= 0 {
		return nil, fmt.Errorf("gitlab: note ID must be > 0, got %d", noteID)
	}
	if body == "" {
		return nil, fmt.Errorf("gitlab: body is required")
	}

	var result *Note
	op := "EditSummary"
	url := fmt.Sprintf("%s/projects/%s/merge_requests/%d/notes/%d",
		c.baseURL, project, iid, noteID)
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		bodyCopy := body
		opts := &gl.UpdateMergeRequestNoteOptions{Body: &bodyCopy}
		note, resp, err := c.inner.Notes.UpdateMergeRequestNote(
			project, int64(iid), noteID, opts)
		if err != nil {
			return c.classify(http.MethodPut, url, resp, err)
		}
		result = projectNote(note)
		c.logger.Debug("gitlab edit ok",
			"op", op,
			"project", project,
			"iid", iid,
			"note_id", noteID,
			"attempt", attempt,
		)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

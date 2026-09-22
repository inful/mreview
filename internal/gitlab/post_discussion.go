package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// PostDiscussion posts an inline discussion thread anchored to
// file:line. The thread can later be replied to, resolved, or
// applied (when Body contains a suggestion block).
//
// The diffRefs are required by GitLab to anchor the comment — every
// inline position MUST carry base_sha, start_sha, head_sha or
// GitLab returns 400.
//
// TODO: collapse into a single shared classifier (issue #3) — this
// classifyPostDiscussionError currently duplicates the
// URL/status/body pattern used by classifyPostSummaryError and the
// four classify*Error helpers in the read paths.
func (c *Client) PostDiscussion(ctx context.Context, project string, iid int, refs DiffRefs, cmt InlineComment) (*Discussion, error) {
	if err := validatePath(project); err != nil {
		return nil, err
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlab: merge request IID must be > 0, got %d", iid)
	}
	if err := validateInlineComment(cmt); err != nil {
		return nil, err
	}
	if err := validateDiffRefs(refs); err != nil {
		return nil, err
	}

	body := cmt.Body
	if cmt.Suggestion != "" {
		// Append a GitLab suggestion block. Markdown fence with the
		// "suggestion" info-string is what GitLab renders as a
		// one-click-apply block.
		body = body + "\n\n```suggestion\n" + cmt.Suggestion + "\n```\n"
	}

	var result *Discussion
	op := "PostDiscussion"
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		file := cmt.File
		oldPath := cmt.OldPath
		newLine := int64(cmt.NewLine)
		oldLine := int64(cmt.OldLine)
		posType := "text"
		opts := &gl.CreateMergeRequestDiscussionOptions{
			Body: &body,
			Position: &gl.PositionOptions{
				BaseSHA:      &refs.BaseSHA,
				StartSHA:     &refs.StartSHA,
				HeadSHA:      &refs.HeadSHA,
				PositionType: &posType,
				NewPath:      &file,
				OldPath:      &oldPath,
				NewLine:      &newLine,
				OldLine:      &oldLine,
			},
		}
		disc, resp, err := c.inner.Discussions.CreateMergeRequestDiscussion(project, int64(iid), opts)
		if err != nil {
			return classifyPostDiscussionError(op, c.baseURL, project, iid, resp, err, body)
		}
		result = projectDiscussion(disc)
		c.logger.Debug("gitlab post ok",
			"op", op,
			"project", project,
			"iid", iid,
			"attempt", attempt,
			"discussion_id", result.ID,
			"file", cmt.File,
			"line", cmt.NewLine,
		)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// classifyPostDiscussionError is the inline-discussion sibling of
// classifyPostSummaryError.
//
// We special-case 400 as KindConflict because GitLab returns 400
// when the line anchor is out of range / the position doesn't match
// a real hunk in the diff — semantically a conflict, not a
// programmer bug. The caller can still inspect the original
// StatusCode via the returned *Error.
func classifyPostDiscussionError(op, baseURL, project string, iid int, resp *gl.Response, err error, body string) error {
	method := http.MethodPost
	url := fmt.Sprintf("%s/projects/%s/merge_requests/%d/discussions", baseURL, project, iid)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	respBody := ""
	// The upstream client drains resp.Body in CheckResponse before
	// returning the *ErrorResponse, so reading resp.Body here
	// returns "". Use the upstream's parsed Message via errors.As
	// when it's available.
	if errResp := asUpstreamError(err); errResp != nil && len(errResp.Body) > 0 {
		respBody = string(errResp.Body)
	} else if errResp != nil && errResp.Message != "" {
		respBody = errResp.Message
	} else if resp != nil {
		respBody = readResponseBody(resp.Body)
	}
	// GitLab returns 400 with "line ... is not a valid line" or
	// "old_line is not in range" when the LLM cites a line outside
	// the file. Classify as conflict so the caller can drop the
	// finding and log it; not a retryable transient.
	wrapped := classifyAndWrap(method, url, status, respBody, err)
	if e := AsError(wrapped); e != nil {
		if e.Kind == KindBadRequest && isLineRangeError(respBody) {
			e.Kind = KindConflict
		}
	}
	return wrapped
}

// asUpstreamError extracts a *gitlab.ErrorResponse from the upstream
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

// isLineRangeError matches the GitLab error strings that signal the
// posted position doesn't match a real hunk. Heuristic — false
// positives fall back to KindBadRequest which is also non-retryable,
// so the worst case is a misleading error class on a malformed
// payload.
func isLineRangeError(body string) bool {
	if body == "" {
		return false
	}
	low := strings.ToLower(body)
	for _, frag := range []string{
		"is not a valid line",
		"is not in range",
		"old_line",
		"new_line",
		"position is not valid",
	} {
		if strings.Contains(low, frag) {
			return true
		}
	}
	return false
}

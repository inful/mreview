package gitlab

import (
	"context"
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
	url := fmt.Sprintf("%s/projects/%s/merge_requests/%d/discussions", c.baseURL, project, iid)
	// GitLab returns 400 with "line ... is not a valid line" or
	// "old_line is not in range" when the LLM cites a line outside
	// the file. Classify as conflict so the caller can drop the
	// finding and log it; not a retryable transient.
	lineRangeAsConflict := func(e *Error) {
		if e.Kind == KindBadRequest && isLineRangeError(e.Body) {
			e.Kind = KindConflict
		}
	}
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		// GitLab's /discussions endpoint requires position fields
		// to match the file status (per the docs):
		//   - New / modified / unchanged line: send new_path +
		//     new_line; send old_path only when it differs from
		//     new_path (renamed files).
		//   - Deleted file / removed line: send old_path + old_line;
		//     omit new_path and new_line entirely.
		//
		// Sending "" or 0 in any field — which the previous code
		// did — makes GitLab return a generic 500 because the
		// internal diff-line lookup fails on the bogus anchor.
		// We use nil pointers (with `omitempty` on the position
		// fields) to drop fields entirely instead.
		//
		// Discrimination between new-side and old-side comments is
		// based on which line field is set: NewLine > 0 means
		// "comment is on the new side", OldLine > 0 means "comment
		// is on the old side". reviewer's buildInlineComment
		// guarantees exactly one of these is set per finding.
		pos := &gl.PositionOptions{
			BaseSHA:      &refs.BaseSHA,
			StartSHA:     &refs.StartSHA,
			HeadSHA:      &refs.HeadSHA,
			PositionType: ptrString("text"),
		}
		switch {
		case cmt.NewLine > 0:
			f := cmt.File
			pos.NewPath = &f
			nl := int64(cmt.NewLine)
			pos.NewLine = &nl
			if cmt.OldPath != "" && cmt.OldPath != cmt.File {
				op := cmt.OldPath
				pos.OldPath = &op
			}
		case cmt.OldLine > 0:
			// cmt.File for deleted files already IS the old path
			// (Chunk.File resolves to NewPath when present, else
			// OldPath). Use cmt.OldPath if set (callers may set it
			// explicitly), else fall back to cmt.File.
			op := cmt.OldPath
			if op == "" {
				op = cmt.File
			}
			pos.OldPath = &op
			ol := int64(cmt.OldLine)
			pos.OldLine = &ol
		}
		opts := &gl.CreateMergeRequestDiscussionOptions{
			Body:     &body,
			Position: pos,
		}
		disc, resp, err := c.inner.Discussions.CreateMergeRequestDiscussion(project, int64(iid), opts)
		if err != nil {
			return c.classify(http.MethodPost, url, resp, err, lineRangeAsConflict)
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

// ptrString returns a pointer to s. Tiny helper to make
// optional-position-field construction in PostDiscussion read cleanly.
func ptrString(s string) *string { return &s }

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

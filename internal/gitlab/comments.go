package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// Note is the subset of *gitlab.Note that the reviewer needs.
//
// Note is what GitLab returns for non-inline MR comments (the summary
// thread) and for individual replies on a discussion.
type Note struct {
	ID         int64  `json:"id"`
	Body       string `json:"body"`
	Author     User   `json:"author"`
	WebURL     string `json:"web_url"`
	System     bool   `json:"system"`
	Resolvable bool   `json:"resolvable"`
}

// Discussion is the subset of *gitlab.Discussion the reviewer needs.
// One Discussion wraps one or more inline comments anchored to the
// same file:line; posting returns the freshly-created thread.
type Discussion struct {
	ID             string `json:"id"`
	IndividualNote bool   `json:"individual_note"`
	Notes          []Note `json:"notes"`
	WebURL         string `json:"web_url"`
}

// InlineComment describes a single comment to post as an inline MR
// discussion thread. The fields mirror the GitLab position object:
//
//   - File         — required: the path at HEAD (new path). For
//     deleted files this is the path the file used to have (since
//     there's no new path).
//   - OldPath      — optional: the path in the base commit. Set when
//     commenting on a modification where old_path differs from new_path
//     (rename, or just to be safe).
//   - NewLine      — line in the new file. 0 means "no new line"
//     (deleted-file comment).
//   - OldLine      — line in the old file. 0 means "no old line"
//     (new-file comment).
//   - Body         — required: the comment text. Markdown supported.
//   - Suggestion   — optional: a fenced code block posted as a
//     "suggestion" block the reviewer can apply with one click.
//     Empty string means no suggestion.
type InlineComment struct {
	File       string
	OldPath    string
	NewLine    int
	OldLine    int
	Body       string
	Suggestion string
}

// PostSummary posts a regular MR note (a top-level comment, not
// anchored to a file/line). This is what the reviewer uses to post
// the overall verdict / summary thread.
//
// Errors are typed via classifyAndWrap; the caller can switch on
// Kind to distinguish auth failures from project locks (409) etc.
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

// projectNote projects an upstream *gitlab.Note onto our flat Note.
//
// Note.Author is a value type (NoteAuthor), so we detect "no
// author" by an empty username — the JSON unmarshaler leaves the
// zero value when the field is absent or null.
//
// A nil *gl.Note is treated as a zero-value Note so callers can
// safely pass through empty upstream results during decoding.
func projectNote(n *gl.Note) *Note {
	if n == nil {
		return &Note{}
	}
	out := &Note{
		ID:         n.ID,
		Body:       n.Body,
		WebURL:     "",
		System:     n.System,
		Resolvable: false, // notes on standalone comments aren't resolvable
	}
	if n.Author.Username != "" {
		out.Author = User{Username: n.Author.Username, Name: n.Author.Name}
	}
	return out
}

// projectDiscussion projects an upstream *gitlab.Discussion onto
// our flat Discussion, recursively projecting the inner notes.
func projectDiscussion(d *gl.Discussion) *Discussion {
	out := &Discussion{
		ID:             d.ID,
		IndividualNote: d.IndividualNote,
		WebURL:         "",
	}
	if len(d.Notes) > 0 {
		out.Notes = make([]Note, 0, len(d.Notes))
		for _, n := range d.Notes {
			pn := projectNote(n)
			out.Notes = append(out.Notes, *pn)
		}
	}
	return out
}

// validatePost checks the inputs shared by PostSummary and any future
// top-level comment endpoint.
func validatePost(project string, iid int, body string) error {
	if err := validatePath(project); err != nil {
		return err
	}
	if iid <= 0 {
		return fmt.Errorf("gitlab: merge request IID must be > 0, got %d", iid)
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("gitlab: note body is empty")
	}
	return nil
}

// validateInlineComment checks the inline-comment-specific invariants.
//
//   - File is required.
//   - At least one of NewLine or OldLine must be > 0 (otherwise the
//     position has no anchor).
//   - Body is required.
func validateInlineComment(cmt InlineComment) error {
	if strings.TrimSpace(cmt.File) == "" {
		return errors.New("gitlab: inline comment file is empty")
	}
	if strings.TrimSpace(cmt.Body) == "" {
		return errors.New("gitlab: inline comment body is empty")
	}
	if cmt.NewLine <= 0 && cmt.OldLine <= 0 {
		return errors.New("gitlab: inline comment requires NewLine or OldLine > 0")
	}
	return nil
}

// validateDiffRefs ensures the diff refs SHAs are present and look
// like SHAs (40 hex chars). GitLab returns 400 with a generic
// message if any is missing, but we want a clearer client-side
// error.
func validateDiffRefs(refs DiffRefs) error {
	if !looksLikeSHA(refs.BaseSHA) || !looksLikeSHA(refs.HeadSHA) || !looksLikeSHA(refs.StartSHA) {
		return fmt.Errorf("gitlab: diff_refs incomplete: base=%q head=%q start=%q",
			refs.BaseSHA, refs.HeadSHA, refs.StartSHA)
	}
	return nil
}

// looksLikeSHA accepts a 40-char hex string. We're permissive about
// case because GitLab SHAs are lowercase but copy/paste happens.
func looksLikeSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
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

package gitlab

import (
	"errors"
	"fmt"
	"strings"
)

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

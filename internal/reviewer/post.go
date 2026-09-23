package reviewer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
	"github.com/inful/mreview/internal/strutil"
)

// postSummary posts the summary note. Honors cfg.DryRun: when
// true, logs the body and returns nil without calling GitLab.
func (r *Reviewer) postSummary(ctx context.Context, project string, iid int, body string) (*gitlab.Note, error) {
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	if r.cfg.DryRun {
		r.cfg.Logger.Info("dry-run: would post summary",
			"project", project,
			"iid", iid,
			"body_bytes", len(body),
		)
		return &gitlab.Note{Body: body}, nil
	}
	note, err := r.cfg.GitLab.PostSummary(ctx, project, iid, body)
	if err != nil {
		return nil, err
	}
	return note, nil
}

// postFinding converts an llm.Finding into an InlineComment and
// posts it as an MR discussion. Returns a PostedFinding whose
// Discussion is nil when the post was skipped (KindConflict,
// file-not-in-diff — already filtered upstream — etc.).
//
// Line-out-of-range errors from GitLab surface as KindConflict
// (gitlab classifier promotes 400-with-line-range-error); the
// finding is dropped with a Reason, not retried.
//
// `meta` carries the file's diff metadata (new / modified /
// deleted / renamed, plus old_path when renamed). The GitLab
// /discussions endpoint requires different position shapes
// depending on file status — see buildInlineComment for the
// per-case rules.
func (r *Reviewer) postFinding(ctx context.Context, project string, iid int, refs gitlab.DiffRefs, meta gitlab.ChangeFile, f llm.Finding) PostedFinding {
	pf := PostedFinding{Finding: f}

	cmt := buildInlineComment(meta, f)

	if r.cfg.DryRun {
		r.cfg.Logger.Info("dry-run: would post inline comment",
			"project", project,
			"iid", iid,
			"file", f.File,
			"line", f.Line,
			"severity", string(f.Severity),
		)
		return pf
	}

	disc, err := r.cfg.GitLab.PostDiscussion(ctx, project, iid, refs, cmt)
	if err != nil {
		pf.Skipped = true
		pf.Reason = classifyPostError(err)
		if shouldFailReview(err) {
			// Auth / not-found / conflict (non-line-range) are
			// caller-fatal. Today we log at Error level and
			// return the per-finding postedFailed so the
			// orchestrator can decide; we don't currently
			// propagate the sentinel up through ReviewMR. A
			// future refinement could short-circuit the loop.
			r.cfg.Logger.Error("inline post failed (fatal)",
				"file", f.File,
				"line", f.Line,
				"err", err.Error(),
			)
		} else {
			r.cfg.Logger.Warn("inline post failed (skipped)",
				"file", f.File,
				"line", f.Line,
				"err", err.Error(),
				"reason", pf.Reason,
			)
		}
		return pf
	}
	pf.Discussion = disc
	return pf
}

// classifyPostError extracts a human-readable reason from a post
// error. Falls back to err.Error() when the error is unknown.
func classifyPostError(err error) string {
	var ge *gitlab.Error
	if errors.As(err, &ge) {
		switch ge.Kind {
		case gitlab.KindConflict:
			return fmt.Sprintf("line out of range or position invalid (status=%d)", ge.StatusCode)
		case gitlab.KindNotFound:
			return "MR or project not found"
		case gitlab.KindAuth:
			return "auth/permission denied"
		case gitlab.KindBadRequest:
			return fmt.Sprintf("bad request: %s", strutil.Truncate(ge.Body, 100))
		case gitlab.KindTransient:
			return "transient failure (retries exhausted)"
		default:
			return fmt.Sprintf("unknown error: %s", strutil.Truncate(ge.Body, 100))
		}
	}
	return strutil.Truncate(err.Error(), 100)
}

// shouldFailReview reports whether a post error should abort the
// whole review rather than just dropping the single finding.
//
// Line-out-of-range (KindConflict) is per-finding — drop and
// continue. Auth / not-found affect every subsequent post — fail
// fast. Transient / bad-request is per-finding.
func shouldFailReview(err error) bool {
	var ge *gitlab.Error
	if !errors.As(err, &ge) {
		return false
	}
	switch ge.Kind {
	case gitlab.KindAuth, gitlab.KindNotFound:
		return true
	default:
		return false
	}
}

// buildInlineComment constructs the InlineComment for one finding,
// choosing the right combination of NewPath / OldPath / NewLine /
// OldLine for the file's diff status. The GitLab /discussions
// endpoint is strict about which fields are sent:
//
//   - New file (NewFile=true): send NewPath + NewLine only. Per
//     the API docs: "To create a thread on an added line, use
//     position[new_line] and don't include position[old_line]."
//     OldPath/OldLine are not omitted by sending "" or 0 — the
//     internal diff-line lookup treats those as "anchor to old
//     line 0", which doesn't exist, and returns 500.
//
//   - Deleted file (DeletedFile=true): send OldPath + OldLine
//     only. There's no new side of the diff to anchor against.
//
//   - Modified or renamed: send NewPath + NewLine (and OldPath
//     only when the rename changed the path — meta.OldPath !=
//     meta.NewPath).
//
// `meta` may be the zero ChangeFile if the finding's file path
// isn't in the diff (shouldn't happen post-filter, but defensive).
// In that case we fall back to the "modified" branch with just
// NewPath + NewLine, which is the most lenient shape.
func buildInlineComment(meta gitlab.ChangeFile, f llm.Finding) gitlab.InlineComment {
	cmt := gitlab.InlineComment{
		File:       f.File,
		Body:       f.Body,
		Suggestion: f.Suggestion,
	}

	switch {
	case meta.NewFile:
		cmt.NewLine = f.Line
		// OldPath and OldLine left zero; PostDiscussion omits them.
	case meta.DeletedFile:
		cmt.OldPath = meta.OldPath
		cmt.OldLine = f.Line
		// NewLine and File's "new side" left zero; PostDiscussion omits.
	default:
		cmt.NewLine = f.Line
		if meta.OldPath != "" && meta.OldPath != meta.NewPath {
			// Renamed file: include the old path so the comment
			// anchors to the rename boundary.
			cmt.OldPath = meta.OldPath
		}
	}
	return cmt
}

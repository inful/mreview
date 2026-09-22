package reviewer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
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
func (r *Reviewer) postFinding(ctx context.Context, project string, iid int, refs gitlab.DiffRefs, f llm.Finding) PostedFinding {
	pf := PostedFinding{Finding: f}

	cmt := gitlab.InlineComment{
		File:       f.File,
		OldPath:    "", // set only for modifications/renames; future enhancement
		NewLine:    f.Line,
		OldLine:    0,
		Body:       f.Body,
		Suggestion: f.Suggestion,
	}

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
			// caller-fatal. Surface via the returned error path
			// — we use a sentinel that the orchestrator unwraps.
			// For now, log loudly; a future refinement would
			// propagate up through ReviewMR.
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
			return fmt.Sprintf("bad request: %s", truncate(ge.Body, 100))
		case gitlab.KindTransient:
			return "transient failure (retries exhausted)"
		default:
			return fmt.Sprintf("unknown error: %s", truncate(ge.Body, 100))
		}
	}
	return truncate(err.Error(), 100)
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

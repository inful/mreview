package reviewer

import (
	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

// Result is what ReviewMR returns to the caller.
//
//   - MR: the GitLab merge-request projection.
//   - Findings: one per llm.Finding the reviewer tried to post.
//     Discussion is nil when the post was skipped (KindConflict,
//     file-not-in-diff, dedupe hit, etc.); Reason carries why.
//   - Summary: the summary note (or nil when --dry-run).
//   - DedupeSize: number of prior bot-authored comments that were
//     used as the dedupe baseline (0 when the MR is fresh).
type Result struct {
	MR         *gitlab.MergeRequest
	Findings   []PostedFinding
	Summary    *gitlab.Note
	DedupeSize int
}

// PostedFinding is the per-finding outcome: what we wanted to post,
// what we did post (or why we skipped it).
type PostedFinding struct {
	Finding    llm.Finding
	Discussion *gitlab.Discussion // nil if skipped
	Skipped    bool
	Reason     string // populated when Skipped
}

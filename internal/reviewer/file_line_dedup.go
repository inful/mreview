package reviewer

import (
	"fmt"
	"log/slog"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/policy"
)

// fileLineSet is the set of "file:line" keys the orchestrator
// builds from prior unresolved findings and matches against
// new findings before posting. The key format is:
//
//	"<path>:<line>"   — anchored inline finding
//	"<path>"          — file-scoped finding (line == 0)
//
// We use this set as a dedup gate: a new finding whose
// file:line matches a prior *unresolved* finding is
// suppressed, because the operator has already been told
// about it. Resolved prior findings don't count — the
// operator marked them obsolete (or accepted them), and
// re-running the review is a chance for the LLM to spot
// something new at the same location.
//
// The design choice is intentional: we do NOT auto-resolve
// prior findings on a new commit. Auto-resolving a finding
// that hasn't actually been fixed is misleading — the
// operator would see "resolved" on something they should
// still be looking at. Resolving happens via the operator
// (or a manual mreview resolve call, never an auto-call).
type fileLineSet struct {
	keys map[string]struct{}
}

// findPriorFindingLocations walks the MR's existing
// discussions and returns the file:line keys for every
// bot-authored inline finding that has not been resolved.
//
// botUsername identifies the bot's own comments; empty
// botUsername treats every comment as the bot's (used in
// tests). System comments and human replies are skipped —
// only the bot's authoritative findings count, and the
// position lives on the first note of the discussion (not
// on subsequent replies).
//
// IndividualNote (the summary post) is skipped — it has
// no file:line and would never match an inline finding.
func findPriorFindingLocations(discs []gitlab.Discussion, botUsername string) *fileLineSet {
	out := &fileLineSet{
		keys: make(map[string]struct{}),
	}
	for _, d := range discs {
		if d.IndividualNote {
			continue
		}
		if d.Resolved {
			continue
		}
		if len(d.Notes) == 0 {
			continue
		}
		// The position lives on the first note; subsequent
		// notes are replies without their own position.
		first := d.Notes[0]
		if botUsername != "" && first.Author.Username != botUsername {
			continue
		}
		if first.Position == nil || first.Position.NewPath == "" {
			continue
		}
		key := first.Position.NewPath
		if first.Position.NewLine > 0 {
			key = fmt.Sprintf("%s:%d", first.Position.NewPath, first.Position.NewLine)
		}
		out.keys[key] = struct{}{}
	}
	return out
}

// Contains reports whether the given finding's file:line is
// in the set. Returns false for an empty set.
func (s *fileLineSet) Contains(f Finding) bool {
	if s == nil {
		return false
	}
	key := f.File
	if f.Line > 0 {
		key = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	_, ok := s.keys[key]
	return ok
}

// Size returns the number of distinct keys tracked.
func (s *fileLineSet) Size() int {
	if s == nil {
		return 0
	}
	return len(s.keys)
}

// dedupFindingsByFileLine drops findings whose file:line
// matches a prior unresolved finding. Returns the survivors
// converted back to the local Finding type (the orchestrator
// works in the local type after this step).
//
// Unlike the old content-fingerprint dedup, this is purely
// semantic — body wording changes between runs don't change
// whether a finding is "the same" as a prior one. The only
// signal is "did the operator already see this location?"
//
// logger may be nil (tests pass nil); the dedup is silent
// in that case.
func dedupFindingsByFileLine(in []policy.EnforcedFinding, prior *fileLineSet, logger *slog.Logger) []Finding {
	out := make([]Finding, 0, len(in))
	for _, f := range in {
		local := Finding{
			File:     f.File,
			Line:     f.Line,
			Severity: Severity(f.Verdict),
			Category: f.Category,
			Body:     f.Body,
		}
		if prior.Contains(local) {
			if logger != nil {
				logger.Debug("dedup: skip finding (already posted at this file:line)",
					"file", f.File, "line", f.Line,
				)
			}
			continue
		}
		out = append(out, local)
	}
	return out
}

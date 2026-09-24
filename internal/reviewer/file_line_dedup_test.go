package reviewer

import (
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/policy"
)

// TestFindPriorFindingLocations_BuildsKeySet exercises the
// happy path: 3 unresolved bot-authored inline findings
// across 3 file:lines, plus one IndividualNote (summary)
// and one human comment, all of which must be skipped.
func TestFindPriorFindingLocations_BuildsKeySet(t *testing.T) {
	discs := []gitlab.Discussion{
		// summary post — should be skipped (no file:line)
		{ID: "d-summary", IndividualNote: true, Notes: []gitlab.Note{
			{ID: 1, Author: gitlab.User{Username: "review-bot"}, Body: "summary"},
		}},
		// inline finding #1
		{ID: "d-1", Notes: []gitlab.Note{
			{ID: 10, Author: gitlab.User{Username: "review-bot"},
				Position: &gitlab.NotePosition{NewPath: "a.go", NewLine: 42}},
		}},
		// inline finding #2 (file-scoped, line 0)
		{ID: "d-2", Notes: []gitlab.Note{
			{ID: 11, Author: gitlab.User{Username: "review-bot"},
				Position: &gitlab.NotePosition{NewPath: "b.go", NewLine: 0}},
		}},
		// inline finding #3 (different line)
		{ID: "d-3", Notes: []gitlab.Note{
			{ID: 12, Author: gitlab.User{Username: "review-bot"},
				Position: &gitlab.NotePosition{NewPath: "a.go", NewLine: 100}},
		}},
		// human comment — should be skipped (wrong author)
		{ID: "d-human", Notes: []gitlab.Note{
			{ID: 20, Author: gitlab.User{Username: "alice"},
				Position: &gitlab.NotePosition{NewPath: "c.go", NewLine: 1}},
		}},
	}

	set := findPriorFindingLocations(discs, "review-bot")

	for _, want := range []string{"a.go:42", "b.go", "a.go:100"} {
		if !containsKey(set, want) {
			t.Errorf("missing key %q; have %v", want, keys(set))
		}
	}
	// Negative: summary + human + empty positions must be absent.
	for _, missing := range []string{"summary", "c.go:1"} {
		if containsKey(set, missing) {
			t.Errorf("unexpected key %q; have %v", missing, keys(set))
		}
	}
	if got := set.Size(); got != 3 {
		t.Errorf("Size = %d, want 3", got)
	}
}

// TestFindPriorFindingLocations_ResolvingSkips pins the
// "auto-resolving prior findings is lying" guard: a prior
// finding the operator marked resolved is NOT in the dedup
// set. We don't want the LLM to skip a real new finding at
// the same location just because the operator dismissed the
// old one.
func TestFindPriorFindingLocations_ResolvingSkips(t *testing.T) {
	discs := []gitlab.Discussion{
		{ID: "d-resolved", Resolved: true, Notes: []gitlab.Note{
			{ID: 10, Author: gitlab.User{Username: "review-bot"},
				Resolved: true,
				Position: &gitlab.NotePosition{NewPath: "a.go", NewLine: 42}},
		}},
	}
	set := findPriorFindingLocations(discs, "review-bot")
	if got := set.Size(); got != 0 {
		t.Errorf("Size = %d, want 0 (resolved finding must NOT be in dedup set); have %v", got, keys(set))
	}
}

// TestFindPriorFindingLocations_NoPosition pins the
// defensive case: an inline comment with no position (the
// upstream omits `position` for replies and for system
// notes) is skipped — we can't form a file:line key without
// it.
func TestFindPriorFindingLocations_NoPosition(t *testing.T) {
	discs := []gitlab.Discussion{
		{ID: "d-no-pos", Notes: []gitlab.Note{
			{ID: 10, Author: gitlab.User{Username: "review-bot"}}, // no Position
		}},
	}
	set := findPriorFindingLocations(discs, "review-bot")
	if got := set.Size(); got != 0 {
		t.Errorf("Size = %d, want 0 (no position is unrepresentable)", got)
	}
}

// TestFindPriorFindingLocations_EmptyBot pins the test
// convenience: empty botUsername treats every comment as
// the bot's. Useful for hermetic tests where the author is
// irrelevant.
func TestFindPriorFindingLocations_EmptyBot(t *testing.T) {
	discs := []gitlab.Discussion{
		{ID: "d-anyone", Notes: []gitlab.Note{
			{ID: 10, Author: gitlab.User{Username: "anyone"},
				Position: &gitlab.NotePosition{NewPath: "x.go", NewLine: 7}},
		}},
	}
	set := findPriorFindingLocations(discs, "") // empty bot
	if got := set.Size(); got != 1 {
		t.Errorf("Size = %d, want 1", got)
	}
}

// TestDedupFindingsByFileLine_MatchSuppressed is the core
// regression test for the user's design: a new finding at a
// file:line that matches an unresolved prior finding must be
// suppressed.
func TestDedupFindingsByFileLine_MatchSuppressed(t *testing.T) {
	prior := findPriorFindingLocations([]gitlab.Discussion{
		{ID: "d-1", Notes: []gitlab.Note{
			{ID: 10, Author: gitlab.User{Username: "review-bot"},
				Position: &gitlab.NotePosition{NewPath: "a.go", NewLine: 42}},
		}},
	}, "review-bot")

	// Same file:line → suppressed.
	in := []policy.EnforcedFinding{
		{File: "a.go", Line: 42, Severity: policy.SeverityWarning, Body: "same issue", Verdict: policy.SeverityWarning},
		// Different file:line → kept.
		{File: "a.go", Line: 43, Severity: policy.SeverityWarning, Body: "new issue", Verdict: policy.SeverityWarning},
	}
	out := dedupFindingsByFileLine(in, prior, nil)
	if len(out) != 1 {
		t.Fatalf("expected 1 surviving finding, got %d", len(out))
	}
	if out[0].File != "a.go" || out[0].Line != 43 {
		t.Errorf("survivor was %s:%d, want a.go:43", out[0].File, out[0].Line)
	}
}

// TestDedupFindingsByFileLine_BodyWordingIsIrrelevant pins
// the semantic-vs-brittle property: body wording changes
// between runs MUST NOT cause a duplicate to be posted. With
// the old content-fingerprint dedup, a wording drift could
// produce two near-duplicate comments at the same file:line.
// File:line dedup closes that hole.
func TestDedupFindingsByFileLine_BodyWordingIsIrrelevant(t *testing.T) {
	prior := findPriorFindingLocations([]gitlab.Discussion{
		{ID: "d-1", Notes: []gitlab.Note{
			{ID: 10, Author: gitlab.User{Username: "review-bot"},
				Body:     "first-run body",
				Position: &gitlab.NotePosition{NewPath: "a.go", NewLine: 42}},
		}},
	}, "review-bot")

	in := []policy.EnforcedFinding{
		{File: "a.go", Line: 42, Severity: policy.SeverityWarning,
			Body: "completely different wording", Verdict: policy.SeverityWarning},
	}
	out := dedupFindingsByFileLine(in, prior, nil)
	if len(out) != 0 {
		t.Errorf("expected 0 survivors (file:line match regardless of body); got %d", len(out))
	}
}

// TestDedupFindingsByFileLine_FileScopedKey covers the
// edge case of Line == 0: a file-scoped finding has no
// specific line; the dedup key is just the file path.
func TestDedupFindingsByFileLine_FileScopedKey(t *testing.T) {
	prior := findPriorFindingLocations([]gitlab.Discussion{
		{ID: "d-1", Notes: []gitlab.Note{
			{ID: 10, Author: gitlab.User{Username: "review-bot"},
				Position: &gitlab.NotePosition{NewPath: "a.go", NewLine: 0}},
		}},
	}, "review-bot")

	in := []policy.EnforcedFinding{
		{File: "a.go", Line: 0, Severity: policy.SeverityInfo,
			Body: "missing test file", Verdict: policy.SeverityInfo},
	}
	out := dedupFindingsByFileLine(in, prior, nil)
	if len(out) != 0 {
		t.Errorf("expected 0 survivors (file-scoped dup); got %d", len(out))
	}
}

// TestDedupFindingsByFileLine_NilPriorIsSafe pins the
// nil-prior path: when no prior summary exists at all
// (cold-start MR), the helper must pass everything through.
func TestDedupFindingsByFileLine_NilPriorIsSafe(t *testing.T) {
	in := []policy.EnforcedFinding{
		{File: "a.go", Line: 1, Severity: policy.SeverityInfo, Body: "x", Verdict: policy.SeverityInfo},
		{File: "a.go", Line: 2, Severity: policy.SeverityInfo, Body: "y", Verdict: policy.SeverityInfo},
	}
	out := dedupFindingsByFileLine(in, nil, nil)
	if len(out) != 2 {
		t.Errorf("expected 2 survivors (nil prior = no dedup); got %d", len(out))
	}
}

// containsKey / keys are tiny test helpers.  We avoid the
// strings.Contains shortcut because the keys embed a colon
// and we want exact-match semantics for unit testing.

func containsKey(s *fileLineSet, k string) bool {
	if s == nil {
		return false
	}
	_, ok := s.keys[k]
	return ok
}

func keys(s *fileLineSet) []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.keys))
	for k := range s.keys {
		out = append(out, k)
	}
	return out
}

// guard against strings import being removed accidentally
var _ = strings.Join

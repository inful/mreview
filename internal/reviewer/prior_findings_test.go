package reviewer

import (
	"log/slog"
	"sort"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
)

// TestExtractPriorFindings_FiltersAndUnpacks pins the
// helper that turns a prior summary's FindingDiscussionIDs
// + the full discussion list into a slice of PriorFinding
// values ready for the LLM prompt + the resolve step.
//
// The interesting cases:
//   - skip summary-style (IndividualNote) discussions
//   - skip non-bot-authored notes
//   - skip notes with no file:line position
//   - skip IDs not present in the discussion list
//     (deleted out-of-band)
func TestExtractPriorFindings_FiltersAndUnpacks(t *testing.T) {
	prior := &PriorReview{
		Commit:               "abc",
		SummaryDiscussionID:  "disc-summary",
		FindingDiscussionIDs: []string{"d1", "d2", "d3", "d4", "d5"},
	}
	discs := []gitlab.Discussion{
		// d1: a real bot-authored inline finding
		{ID: "d1", Notes: []gitlab.Note{{
			ID: 100, Body: "real finding",
			Author:   gitlab.User{Username: "mreview-bot"},
			Position: &gitlab.NotePosition{NewPath: "a.go", NewLine: 10},
		}}},
		// d2: summary-style (IndividualNote=true) — skip
		{ID: "d2", IndividualNote: true, Notes: []gitlab.Note{{
			ID: 101, Body: "summary",
		}}},
		// d3: human comment — skip (wrong author)
		{ID: "d3", Notes: []gitlab.Note{{
			ID: 102, Body: "human",
			Author:   gitlab.User{Username: "alice"},
			Position: &gitlab.NotePosition{NewPath: "b.go", NewLine: 20},
		}}},
		// d4: no position — skip
		{ID: "d4", Notes: []gitlab.Note{{
			ID: 103, Body: "no position",
			Author: gitlab.User{Username: "mreview-bot"},
		}}},
		// d5: not in the discussion list (id present
		// in prior.FindingDiscussionIDs but disc with
		// that id not in discs) — skip silently
	}
	got := extractPriorFindings(prior, discs, "mreview-bot")
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1 (only d1 should pass)", len(got))
	}
	if got[0].File != "a.go" || got[0].Line != 10 {
		t.Errorf("got %s:%d, want a.go:10", got[0].File, got[0].Line)
	}
	if got[0].DiscussionID != "d1" {
		t.Errorf("DiscussionID = %q, want d1", got[0].DiscussionID)
	}
	if got[0].NoteID != 100 {
		t.Errorf("NoteID = %d, want 100", got[0].NoteID)
	}
	if got[0].Body != "real finding" {
		t.Errorf("Body = %q, want %q", got[0].Body, "real finding")
	}
}

// TestExtractPriorFindings_NilOrEmpty covers the
// defensive paths: no prior, no IDs.
func TestExtractPriorFindings_NilOrEmpty(t *testing.T) {
	if got := extractPriorFindings(nil, nil, ""); got != nil {
		t.Errorf("nil prior: got %v, want nil", got)
	}
	if got := extractPriorFindings(&PriorReview{}, nil, ""); got != nil {
		t.Errorf("empty FindingDiscussionIDs: got %v, want nil", got)
	}
}

// TestClassifyPriorFindings_StillValid covers the
// "LLM said still_valid" path. The prior finding is
// kept; the GitLab DiscussionID is recorded for the
// next-run marker.
func TestClassifyPriorFindings_StillValid(t *testing.T) {
	priors := []PriorFinding{
		{File: "a.go", Line: 10, DiscussionID: "d1"},
	}
	llm := []PriorFindingStatus{
		{File: "a.go", Line: 10, Status: StatusStillValid},
	}
	got := classifyPriorFindings(llm, priors, slog.Default())
	if len(got.carryOver) != 1 || got.carryOver[0].DiscussionID != "d1" {
		t.Errorf("carryOver = %+v, want one entry with DiscussionID d1", got.carryOver)
	}
	if len(got.resolve) != 0 {
		t.Errorf("resolve = %+v, want empty", got.resolve)
	}
	if len(got.carryOverIDs) != 1 || got.carryOverIDs[0] != "d1" {
		t.Errorf("carryOverIDs = %+v, want [d1]", got.carryOverIDs)
	}
}

// TestClassifyPriorFindings_Resolved pins the "LLM said
// resolved" path. The orchestrator should resolve the
// prior discussion and pass the rationale through to
// the table renderer.
func TestClassifyPriorFindings_Resolved(t *testing.T) {
	priors := []PriorFinding{
		{File: "a.go", Line: 10, DiscussionID: "d1"},
	}
	llm := []PriorFindingStatus{
		{File: "a.go", Line: 10, Status: StatusResolved, Rationale: "test added"},
	}
	got := classifyPriorFindings(llm, priors)
	if len(got.resolve) != 1 || got.resolve[0].DiscussionID != "d1" {
		t.Errorf("resolve = %+v, want one entry with DiscussionID d1", got.resolve)
	}
	if len(got.carryOver) != 0 {
		t.Errorf("carryOver = %+v, want empty", got.carryOver)
	}
	if len(got.resolveStatuses) != 1 || got.resolveStatuses[0] != StatusResolved {
		t.Errorf("resolveStatuses = %+v, want [resolved]", got.resolveStatuses)
	}
	if len(got.resolveRationale) != 1 || got.resolveRationale[0] != "test added" {
		t.Errorf("resolveRationale = %+v, want [test added]", got.resolveRationale)
	}
}

// TestClassifyPriorFindings_OutOfScope is the same
// as resolved but with the out_of_scope status — both
// statuses auto-resolve the discussion but the table
// shows a different label.
func TestClassifyPriorFindings_OutOfScope(t *testing.T) {
	priors := []PriorFinding{
		{File: "a.go", Line: 10, DiscussionID: "d1"},
	}
	llm := []PriorFindingStatus{
		{File: "a.go", Line: 10, Status: StatusOutOfScope, Rationale: "file deleted"},
	}
	got := classifyPriorFindings(llm, priors)
	if len(got.resolve) != 1 {
		t.Errorf("resolve = %+v, want one entry", got.resolve)
	}
	if got.resolveStatuses[0] != StatusOutOfScope {
		t.Errorf("resolveStatuses[0] = %q, want out_of_scope", got.resolveStatuses[0])
	}
}

// TestClassifyPriorFindings_LLMOmitsEntry covers the
// defensive default: if the LLM didn't mention a prior
// finding in its prior_findings array, treat it as
// still_valid. Silence from the LLM is NOT consent to
// auto-resolve — the operator should still see the
// prior finding in the table.
func TestClassifyPriorFindings_LLMOmitsEntry(t *testing.T) {
	priors := []PriorFinding{
		{File: "a.go", Line: 10, DiscussionID: "d1"},
		{File: "b.go", Line: 20, DiscussionID: "d2"},
	}
	// LLM only mentions d1; d2 is omitted.
	llm := []PriorFindingStatus{
		{File: "a.go", Line: 10, Status: StatusStillValid},
	}
	got := classifyPriorFindings(llm, priors)
	if len(got.carryOver) != 2 {
		t.Errorf("carryOver = %+v, want 2 (both priors, the omitted one defaults to still_valid)", got.carryOver)
	}
	if len(got.resolve) != 0 {
		t.Errorf("resolve = %+v, want empty", got.resolve)
	}
	// Verify the order matches the input order
	// (d1, then d2 — not alphabetised or anything).
	ids := []string{got.carryOver[0].DiscussionID, got.carryOver[1].DiscussionID}
	if !sort.StringsAreSorted(ids) && ids[0] != "d1" {
		t.Errorf("carryOver order = %v, want [d1, d2]", ids)
	}
}

// TestClassifyPriorFindings_UnknownStatus covers the
// defensive default: an unknown status value (e.g. the
// LLM mis-emitted or a future schema change isn't
// recognised) is treated as still_valid and logged.
// The test passes nil logger to keep it quiet.
func TestClassifyPriorFindings_UnknownStatus(t *testing.T) {
	priors := []PriorFinding{
		{File: "a.go", Line: 10, DiscussionID: "d1"},
	}
	llm := []PriorFindingStatus{
		{File: "a.go", Line: 10, Status: "ambiguous"},
	}
	got := classifyPriorFindings(llm, priors)
	if len(got.carryOver) != 1 {
		t.Errorf("carryOver = %+v, want 1 (unknown status defaults to still_valid)", got.carryOver)
	}
	if len(got.resolve) != 0 {
		t.Errorf("resolve = %+v, want empty (unknown status should not auto-resolve)", got.resolve)
	}
}

// TestFindingStatus_DisplayLabelAndEmoji pins the
// operator-facing labels and emojis for each status.
// These are user-visible strings, so a regression is
// worth a dedicated test.
func TestFindingStatus_DisplayLabelAndEmoji(t *testing.T) {
	cases := []struct {
		status   FindingStatus
		label    string
		emoji    string
		resolved bool
	}{
		{StatusNew, "new", "🆕", false},
		{StatusStillValid, "carried over", "↻", false},
		{StatusResolved, "resolved", "✓", true},
		{StatusOutOfScope, "out of scope", "∅", true},
	}
	for _, c := range cases {
		if got := c.status.DisplayLabel(); got != c.label {
			t.Errorf("%q: DisplayLabel() = %q, want %q", c.status, got, c.label)
		}
		if got := c.status.Emoji(); got != c.emoji {
			t.Errorf("%q: Emoji() = %q, want %q", c.status, got, c.emoji)
		}
		if got := c.status.IsResolved(); got != c.resolved {
			t.Errorf("%q: IsResolved() = %v, want %v", c.status, got, c.resolved)
		}
	}
}

// TestPriorKey covers the (file, line) → key helper
// used to match LLM statuses back to prior findings.
// File-scoped findings (line == 0) use just the file
// path so a status reply with line=0 still matches.
func TestPriorKey(t *testing.T) {
	cases := []struct {
		file string
		line int
		want string
	}{
		{"a.go", 10, "a.go:10"},
		{"a.go", 0, "a.go"}, // file-scoped
		{"path/with/slashes.go", 99, "path/with/slashes.go:99"},
		{"x.go", 1, "x.go:1"},
	}
	for _, c := range cases {
		if got := priorKey(c.file, c.line); got != c.want {
			t.Errorf("priorKey(%q, %d) = %q, want %q", c.file, c.line, got, c.want)
		}
	}
}

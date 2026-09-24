package reviewer

import (
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
)

// TestFormatMarkerLine pins the format. This is the marker
// the next mreview run reads back — change the format and
// every prior run becomes invisible to the new dedup logic.
func TestFormatMarkerLine(t *testing.T) {
	cases := []struct {
		name     string
		commit   string
		findings []string
		want     string
	}{
		{
			name:     "single finding",
			commit:   "abc123def",
			findings: []string{"d-1"},
			want:     "<!-- mreview:commit=abc123def findings=d-1 -->",
		},
		{
			name:     "multiple findings",
			commit:   "deadbeef",
			findings: []string{"d-1", "d-2", "d-3"},
			want:     "<!-- mreview:commit=deadbeef findings=d-1,d-2,d-3 -->",
		},
		{
			name:     "no findings (clean review)",
			commit:   "fff",
			findings: nil,
			want:     "<!-- mreview:commit=fff -->",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatMarkerLine(tc.commit, tc.findings)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseMarkerLine_RoundTrip confirms FormatMarkerLine +
// ParseMarkerLine are inverses. This is the contract that
// makes the dedup flow work.
func TestParseMarkerLine_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		commit   string
		findings []string
	}{
		{"single", "abc123", []string{"d1"}},
		{"multi", "deadbeef", []string{"d1", "d2", "d3"}},
		{"empty findings", "fff", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := FormatMarkerLine(tc.commit, tc.findings)
			prior := ParseMarkerLine(line)
			if prior == nil {
				t.Fatalf("parse returned nil for %q", line)
			}
			if prior.Commit != tc.commit {
				t.Errorf("commit = %q, want %q", prior.Commit, tc.commit)
			}
			if len(prior.FindingDiscussionIDs) != len(tc.findings) {
				t.Errorf("findings len = %d, want %d", len(prior.FindingDiscussionIDs), len(tc.findings))
				return
			}
			for i := range tc.findings {
				if prior.FindingDiscussionIDs[i] != tc.findings[i] {
					t.Errorf("findings[%d] = %q, want %q", i, prior.FindingDiscussionIDs[i], tc.findings[i])
				}
			}
		})
	}
}

// TestParseMarkerLine_NoMarker pins the "no marker in body"
// path: returns nil (caller treats as "fresh review").
func TestParseMarkerLine_NoMarker(t *testing.T) {
	bodies := []string{
		"",
		"just a regular comment",
		"# mreview summary\n\nFindings...",  // missing marker
		"<!-- not a marker -->",
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			if got := ParseMarkerLine(body); got != nil {
				t.Errorf("body %q should not parse, got %+v", body, got)
			}
		})
	}
}

// TestParseMarkerLine_CommitOnly confirms a marker with
// only the commit field (no findings) parses correctly.
// This is the clean-review shape — `FormatMarkerLine`
// emits it when the run produced zero inline findings.
func TestParseMarkerLine_CommitOnly(t *testing.T) {
	prior := ParseMarkerLine("<!-- mreview:commit=abc123 -->")
	if prior == nil {
		t.Fatal("parse returned nil for commit-only marker")
	}
	if prior.Commit != "abc123" {
		t.Errorf("commit = %q, want abc123", prior.Commit)
	}
	if len(prior.FindingDiscussionIDs) != 0 {
		t.Errorf("findings = %v, want empty", prior.FindingDiscussionIDs)
	}
}

// TestParseMarkerLine_EmbeddedInBody confirms the marker
// can sit anywhere in the body, not just at the start. The
// orchestrator writes the marker first, but hand-edited
// bodies (or summaries that grew a prefix) should still
// parse.
func TestParseMarkerLine_EmbeddedInBody(t *testing.T) {
	body := "# mreview summary\n\n" + FormatMarkerLine("cafe", []string{"d-a", "d-b"}) + "\n\nFindings follow…"
	prior := ParseMarkerLine(body)
	if prior == nil {
		t.Fatal("parse returned nil for body with embedded marker")
	}
	if prior.Commit != "cafe" {
		t.Errorf("commit = %q, want cafe", prior.Commit)
	}
	if got := strings.Join(prior.FindingDiscussionIDs, ","); got != "d-a,d-b" {
		t.Errorf("findings = %q, want d-a,d-b", got)
	}
}

// TestFindPriorMReviewSummary_ScansDiscussions exercises
// the discovery path against a synthetic discussion list.
// The newest-first ordering means the FIRST match wins —
// if a developer manually edited an older summary and the
// regex is permissive enough to match a hand-typed body,
// we'd surface the most recent one.
func TestFindPriorMReviewSummary_ScansDiscussions(t *testing.T) {
	// 4 discussions, 2 with markers (the most recent
	// mreview-run marker is in discussions[1]; discussions[3]
	// has an older marker).
	discussions := []gitlab.Discussion{
		// 0 — unrelated comment
		{ID: "d0", Notes: []gitlab.Note{{Body: "Hello from a human."}}},
		// 1 — most recent mreview run (should be picked)
		{ID: "d1", Notes: []gitlab.Note{{Body: FormatMarkerLine("new-sha", []string{"d1-a", "d1-b"}) + "\n# mreview summary" + "\nFindings follow…"}}},
		// 2 — unrelated
		{ID: "d2", Notes: []gitlab.Note{{Body: "Another human note."}}},
		// 3 — older mreview run, should be ignored
		{ID: "d3", Notes: []gitlab.Note{{Body: FormatMarkerLine("old-sha", nil)}}},
	}
	prior := FindPriorMReviewSummary(discussions)
	if prior == nil {
		t.Fatal("expected to find a prior summary")
	}
	if prior.Commit != "new-sha" {
		t.Errorf("commit = %q, want new-sha (most recent wins)", prior.Commit)
	}
	if prior.SummaryDiscussionID != "d1" {
		t.Errorf("SummaryDiscussionID = %q, want d1", prior.SummaryDiscussionID)
	}
	if got := strings.Join(prior.FindingDiscussionIDs, ","); got != "d1-a,d1-b" {
		t.Errorf("findings = %q, want d1-a,d1-b", got)
	}
}

// TestFindPriorMReviewSummary_NoMarker confirms the
// no-prior-found path returns nil (caller proceeds with
// fresh review).
func TestFindPriorMReviewSummary_NoMarker(t *testing.T) {
	discussions := []gitlab.Discussion{
		{ID: "d0", Notes: []gitlab.Note{{Body: "Just a comment."}}},
		{ID: "d1", Notes: []gitlab.Note{{Body: "Another comment."}}},
	}
	if got := FindPriorMReviewSummary(discussions); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

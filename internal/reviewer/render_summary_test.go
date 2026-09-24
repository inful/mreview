package reviewer

import (
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/policy"
)

// TestRenderSummary_EmptyFindings pins the "no issues" path
// so a future regression that prints `<details>` followed
// by an empty table fails loudly (the "no issues found"
// sentinel is the operator's only signal that mreview ran
// successfully).
func TestRenderSummary_EmptyFindings(t *testing.T) {
	mr := &gitlab.MergeRequest{Title: "Test MR", SHA: "sha-1"}
	out := renderSummary(mr, "Looks fine to me.", nil, nil)

	for _, want := range []string{
		"# mreview summary",
		"Looks fine to me.",
		"<details>",
		"<summary>Findings (0)</summary>",
		"No issues found.",
		"</details>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// And critically: no markdown-table header in the empty
	// path — that'd render as an empty table in GitLab.
	if strings.Contains(out, "| Severity |") {
		t.Errorf("empty findings should not render an empty table; got:\n%s", out)
	}
}

// TestRenderSummary_SingleFinding pins the row format with
// the four-column table. This is the failure-mode test
// for the "all squashed together" GitLab rendering bug.
func TestRenderSummary_SingleFinding(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	findings := []policy.EnforcedFinding{
		{
			File:     "cmd/main.go",
			Line:     42,
			Severity: policy.SeverityError,
			Category: "correctness",
			Body:     "Does the thing wrong.",
			Verdict:  policy.SeverityError,
		},
	}
	out := renderSummary(mr, "", findings, []string{"d-a"})

	// The header row is GitLab's signal that this is a table.
	for _, want := range []string{
		"| Severity | File | Line | Category | Description |",
		"|----------|------|-----:|----------|-------------|", // right-aligned line numbers
		"| 🛑 error | `cmd/main.go` | 42 | correctness | Does the thing wrong. |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

// TestRenderSummary_MultipleFindings confirms each finding
// becomes its own row (no squashing into a single line).
func TestRenderSummary_MultipleFindings(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	findings := []policy.EnforcedFinding{
		{File: "a.go", Line: 1, Severity: policy.SeverityError, Category: "security", Body: "Hard-coded secret.", Verdict: policy.SeverityError},
		{File: "b.go", Line: 7, Severity: policy.SeverityWarning, Category: "perf", Body: "O(n^2) loop.", Verdict: policy.SeverityWarning},
		{File: "c.go", Line: 13, Severity: policy.SeverityInfo, Category: "style", Body: "Naming nit.", Verdict: policy.SeverityInfo},
	}
	out := renderSummary(mr, "", findings, []string{"d-a", "d-b", "d-c"})

	// Each finding must occupy its own line; if any two ended
	// up on the same line, the table render in GitLab would
	// collapse to one row.
	for _, want := range []string{
		"🛑 error | `a.go`",
		"⚠️ warning | `b.go`",
		"ℹ️ info | `c.go`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

// TestRenderSummary_BodyWithNewlines pins the <br>
// in-cell line break behaviour. GitLab tables collapse
// newlines into a single space inside a cell unless we
// emit an explicit <br>; without this fix the body becomes
// an unreadable wall.
func TestRenderSummary_BodyWithNewlines(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	findings := []policy.EnforcedFinding{
		{
			File:     "x.go",
			Line:     1,
			Severity: policy.SeverityWarning,
			Category: "correctness",
			Body:     "First sentence.\nSecond sentence.\nThird sentence.",
			Verdict:  policy.SeverityWarning,
		},
	}
	out := renderSummary(mr, "", findings, nil)

	if !strings.Contains(out, "First sentence.<br>Second sentence.<br>Third sentence.") {
		t.Errorf("newlines should be replaced with <br>; got:\n%s", out)
	}
	// And the body still contains no bare \n inside the cell
	// (which would have been the old broken behaviour).
	if strings.Contains(out, "First sentence.\nSecond") {
		t.Errorf("body still has bare \\n inside table cell; got:\n%s", out)
	}
}

// TestRenderSummary_BodyWithPipes pins the backslash-escape
// for `|` characters in the body. Without the escape, a
// pipe inside the body would terminate the table row and
// break the markdown structure downstream.
func TestRenderSummary_BodyWithPipes(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	findings := []policy.EnforcedFinding{
		{
			File:     "x.go",
			Line:     1,
			Severity: policy.SeverityInfo,
			Category: "style",
			Body:     "Use `a | b` rather than the alternative.",
			Verdict:  policy.SeverityInfo,
		},
	}
	out := renderSummary(mr, "", findings, nil)

	want := "Use `a \\| b` rather than the alternative."
	if !strings.Contains(out, want) {
		t.Errorf("pipe should be backslash-escaped; want %q; got:\n%s", want, out)
	}
}

// TestRenderSummary_VerdictOverridesSeverity pins the
// emoji-from-Verdict behaviour: a finding whose Severity
// was "info" but whose Verdict was escalated to "error"
// (via policy.severity_override) should render with the
// error emoji.
func TestRenderSummary_VerdictOverridesSeverity(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	findings := []policy.EnforcedFinding{
		{
			File:     "x.go",
			Line:     1,
			Severity: policy.SeverityInfo,
			Category: "security",
			Body:     "Escalated by policy.",
			Verdict:  policy.SeverityError,
		},
	}
	out := renderSummary(mr, "", findings, nil)

	if !strings.Contains(out, "| 🛑 error |") {
		t.Errorf("verdict (not severity) should drive emoji; got:\n%s", out)
	}
}

// TestRenderSummary_SummaryAndFindings confirms the full
// layout: a top heading, an optional summary paragraph,
// then the <details>-wrapped table.
func TestRenderSummary_SummaryAndFindings(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	findings := []policy.EnforcedFinding{
		{File: "x.go", Line: 1, Severity: policy.SeverityWarning, Category: "test", Body: "Missing test for foo.", Verdict: policy.SeverityWarning},
	}
	out := renderSummary(mr, "Overall LGTM.", findings, []string{"d-x"})

	wantOrder := []string{
		"# mreview summary",
		"\nOverall LGTM.\n",
		"<details>",
		"<summary>Findings (1)</summary>",
		"| Severity |",
		"| ⚠️ warning | `x.go` | 1 | test | Missing test for foo. |",
		"</details>",
	}
	last := 0
	for _, want := range wantOrder {
		idx := strings.Index(out, want)
		if idx < 0 {
			t.Errorf("missing %q in output:\n%s", want, out)
			continue
		}
		if idx < last {
			t.Errorf("order wrong: %q appeared at %d, expected after %d", want, idx, last)
		}
		last = idx
	}
}

// TestRenderSummary_MarkerPrepended pins the dedup marker
// at the top of the rendered body when mr.SHA is non-empty.
// The marker is the wire format the next mreview run reads
// to detect prior reviews; if its format or position drifts,
// the dedup flow silently regresses to "no prior summary
// found, post fresh every time".
func TestRenderSummary_MarkerPrepended(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "abc123def"}
	out := renderSummary(mr, "", nil, nil)
	want := "<!-- mreview:commit=abc123def -->"
	if !strings.Contains(out, want) {
		t.Errorf("commit-only marker not present; got:\n%s", out)
	}
	// Marker must come BEFORE the body — operator-visible
	// content stays at top.
	markerIdx := strings.Index(out, want)
	bodyIdx := strings.Index(out, "# mreview summary")
	if !(markerIdx >= 0 && bodyIdx > markerIdx) {
		t.Errorf("ordering wrong: marker=%d body=%d\n%s", markerIdx, bodyIdx, out)
	}
}

// TestRenderSummary_MarkerWithFindings pins the marker
// shape when there ARE findings: commit + comma-separated
// finding IDs at the very top.
func TestRenderSummary_MarkerWithFindings(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "deadbeef"}
	out := renderSummary(mr, "", nil, []string{"d-1", "d-2", "d-3"})
	want := "<!-- mreview:commit=deadbeef findings=d-1,d-2,d-3 -->"
	if !strings.Contains(out, want) {
		t.Errorf("findings-bearing marker not present; got:\n%s", out)
	}
}

// TestRenderSummary_NoMarkerWhenSHAEmpty confirms the
// fallback: an MR whose projection doesn't carry SHA
// (older deployments, API edge case) renders without a
// marker. The next run can't dedup, but it can still post.
func TestRenderSummary_NoMarkerWhenSHAEmpty(t *testing.T) {
	mr := &gitlab.MergeRequest{} // SHA: ""
	out := renderSummary(mr, "", nil, []string{"d-1"})
	if strings.Contains(out, "<!-- mreview:") {
		t.Errorf("expected no marker when SHA is empty; got:\n%s", out)
	}
}

// TestRenderSummary_DetailsWrapping pins the new
// <details>/<summary> structure end-to-end. This is the
// regression test for the rendering change that puts the
// finding count in <summary> and the table inside <details>,
// so the GitLab message defaults to a single click-to-expand
// row instead of dumping the whole table into the MR's
// activity feed.
func TestRenderSummary_DetailsWrapping(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	findings := []policy.EnforcedFinding{
		{File: "a.go", Line: 1, Severity: policy.SeverityError, Category: "x", Body: "one", Verdict: policy.SeverityError},
		{File: "b.go", Line: 2, Severity: policy.SeverityError, Category: "x", Body: "two", Verdict: policy.SeverityError},
		{File: "c.go", Line: 3, Severity: policy.SeverityError, Category: "x", Body: "three", Verdict: policy.SeverityError},
	}
	out := renderSummary(mr, "", findings, nil)

	// Single <details> block that opens before the table and
	// closes after it. Critical: no nested or stray
	// <details>/</details>.
	openCount := strings.Count(out, "<details>")
	closeCount := strings.Count(out, "</details>")
	if openCount != 1 {
		t.Errorf("expected exactly 1 <details> open; got %d in:\n%s", openCount, out)
	}
	if closeCount != 1 {
		t.Errorf("expected exactly 1 </details> close; got %d in:\n%s", closeCount, out)
	}

	// Summary line must carry the count.
	if !strings.Contains(out, "<summary>Findings (3)</summary>") {
		t.Errorf("summary must carry the count; got:\n%s", out)
	}

	// Ordering: <details> opens, summary follows, blank
	// line, table, blank line, </details>. We assert the
	// table sits BETWEEN the <summary> and </details>.
	openIdx := strings.Index(out, "<details>")
	summaryIdx := strings.Index(out, "<summary>Findings (3)</summary>")
	tableIdx := strings.Index(out, "| Severity | File | Line | Category | Description |")
	closeIdx := strings.Index(out, "</details>")
	if !(openIdx < summaryIdx && summaryIdx < tableIdx && tableIdx < closeIdx) {
		t.Errorf("ordering wrong: <details>=%d <summary>=%d <table>=%d </details>=%d\n%s",
			openIdx, summaryIdx, tableIdx, closeIdx, out)
	}

	// The blank line between </summary> and the table is
	// critical — without it, GitLab's markdown parser does
	// not recognise the table block. Pin it explicitly.
	if !strings.Contains(out, "</summary>\n\n| Severity |") {
		t.Errorf("missing blank line between </summary> and the table header; got:\n%s", out)
	}
}

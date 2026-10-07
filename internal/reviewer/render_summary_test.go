package reviewer

import (
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
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
	// The new Status column header should not appear
	// either — a header row without a body is a GitLab
	// render trap.
	if strings.Contains(out, "| Status |") {
		t.Errorf("empty findings should not render a header row; got:\n%s", out)
	}
}

// TestRenderSummary_SingleFinding pins the row format with
// the five-column table. The Status column is the new piece
// (🆕 new for a freshly-posted finding).
func TestRenderSummary_SingleFinding(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	rows := []SummaryRow{
		{
			File:     "cmd/main.go",
			Line:     42,
			Severity: "error",
			Category: "correctness",
			Body:     "Does the thing wrong.",
			Status:   StatusNew,
		},
	}
	out := renderSummary(mr, "", rows, []string{"d-a"})

	// The header row is GitLab's signal that this is a table.
	for _, want := range []string{
		"| Status | Severity | File | Line | Category | Description |",
		"|--------|----------|------|-----:|----------|-------------|", // right-aligned line numbers
		"| 🆕 new | 🛑 error | `cmd/main.go` | 42 | correctness | Does the thing wrong. |",
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
	rows := []SummaryRow{
		{File: "a.go", Line: 1, Severity: "error", Category: "security", Body: "Hard-coded secret.", Status: StatusNew},
		{File: "b.go", Line: 7, Severity: "warning", Category: "perf", Body: "O(n^2) loop.", Status: StatusNew},
		{File: "c.go", Line: 13, Severity: "info", Category: "style", Body: "Naming nit.", Status: StatusNew},
	}
	out := renderSummary(mr, "", rows, []string{"d-a", "d-b", "d-c"})

	// Each finding must occupy its own line; if any two ended
	// up on the same line, the table render in GitLab would
	// collapse to one row.
	for _, want := range []string{
		"🆕 new | 🛑 error | `a.go`",
		"🆕 new | ⚠️ warning | `b.go`",
		"🆕 new | ℹ️ info | `c.go`",
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
	rows := []SummaryRow{
		{
			File:     "x.go",
			Line:     1,
			Severity: "warning",
			Category: "correctness",
			Body:     "First sentence.\nSecond sentence.\nThird sentence.",
			Status:   StatusNew,
		},
	}
	out := renderSummary(mr, "", rows, nil)

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
	rows := []SummaryRow{
		{
			File:     "x.go",
			Line:     1,
			Severity: "info",
			Category: "style",
			Body:     "Use `a | b` rather than the alternative.",
			Status:   StatusNew,
		},
	}
	out := renderSummary(mr, "", rows, nil)

	want := "Use `a \\| b` rather than the alternative."
	if !strings.Contains(out, want) {
		t.Errorf("pipe should be backslash-escaped; want %q; got:\n%s", want, out)
	}
}

// TestRenderSummary_CarriedOver covers the carried-over
// row shape: same Status column (with "carried over"
// label and ↻ glyph), Severity/Category placeholders
// ("(prior)" so the operator knows the original is one
// click away), the prior body verbatim.
func TestRenderSummary_CarriedOver(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	rows := []SummaryRow{
		{
			File:     "x.go",
			Line:     10,
			Severity: "warning", // ignored for carried-over
			Category: "test",    // ignored for carried-over
			Body:     "Missing test for foo.",
			Status:   StatusStillValid,
		},
	}
	out := renderSummary(mr, "", rows, []string{"d-existing"})

	for _, want := range []string{
		"↻ carried over",
		"(prior)",
		"Missing test for foo.",
		// The "warning" text should NOT appear in the
		// Severity column for carried-over rows —
		// that would be misleading (the verdict
		// applied to the original review, not this
		// carried-over status).
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in carried-over output:\n%s", want, out)
		}
	}
	// Defence-in-depth: the original severity text
	// should be replaced by the "(prior)" placeholder.
	// We don't assert that "warning" is absent (it
	// could appear in the body) but we do check the
	// Severity cell shape.
	if !strings.Contains(out, "↻ carried over | (prior) |") {
		t.Errorf("Severity column for carried-over should be '(prior)'; got:\n%s", out)
	}
}

// TestRenderSummary_Resolved covers the resolved row
// shape: Status column shows "resolved" + ✓ glyph, the
// LLM's rationale is appended to the body in an
// "Resolved: ..." suffix so the operator can see WHY
// the prior finding was auto-resolved without
// clicking through to the (now-collapsed) thread.
func TestRenderSummary_Resolved(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	rows := []SummaryRow{
		{
			File:      "x.go",
			Line:      10,
			Severity:  "warning",
			Category:  "test",
			Body:      "Missing test for foo.",
			Status:    StatusResolved,
			Rationale: "The test was added on line 12 of the same file.",
		},
	}
	out := renderSummary(mr, "", rows, nil)

	for _, want := range []string{
		"✓ resolved",
		"Missing test for foo.",
		// Rationale is rendered as "Resolved: <rationale>"
		// in italics so it visually separates from the
		// body and from the column header.
		"Resolved: The test was added on line 12 of the same file.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in resolved output:\n%s", want, out)
		}
	}
}

// TestRenderSummary_OutOfScope covers the out_of_scope
// row: same display shape as resolved (auto-collapsed
// thread, "resolved" label) but with the ∅ glyph and
// "out of scope" label, plus the rationale.
func TestRenderSummary_OutOfScope(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	rows := []SummaryRow{
		{
			File:      "old.go",
			Line:      10,
			Severity:  "warning",
			Category:  "test",
			Body:      "Missing test for foo.",
			Status:    StatusOutOfScope,
			Rationale: "The file was deleted in this MR.",
		},
	}
	out := renderSummary(mr, "", rows, nil)

	for _, want := range []string{
		"∅ out of scope",
		"Resolved: The file was deleted in this MR.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in out_of_scope output:\n%s", want, out)
		}
	}
}

// TestRenderSummary_MixedStatus covers the realistic
// follow-up-run case: a mix of new, carried-over, and
// resolved findings in one table. The order is determined
// by buildSummaryRows (new → carried-over → resolved,
// with (file, line) sorting inside each group). The test
// drives through buildSummaryRows to keep the sort logic
// in one place — renderSummary itself is order-agnostic
// and just renders whatever rows it gets.
func TestRenderSummary_MixedStatus(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	// Build the input slices as the orchestrator would
	// after classifyPriorFindings:
	//   - newFindings: from the LLM's resp.Findings
	//   - carryOver: the prior findings the LLM said
	//     still_valid
	//   - resolved: the prior findings the LLM said
	//     resolved or out_of_scope
	newFindings := []Finding{
		{File: "c.go", Line: 30, Severity: SeverityInfo, Category: "style", Body: "Naming nit."},
		{File: "d.go", Line: 5, Severity: SeverityError, Category: "correctness", Body: "New issue."},
	}
	carryOver := []PriorFinding{
		{File: "b.go", Line: 20, Body: "O(n^2) loop."},
	}
	resolved := []priorResolvedEntry{
		{Finding: PriorFinding{File: "a.go", Line: 10, Body: "Hard-coded secret."}, Status: StatusResolved, Rationale: "moved to env var"},
	}
	rows := buildSummaryRows(newFindings, carryOver, resolved)
	out := renderSummary(mr, "", rows, nil)

	// The four rows must appear in the documented
	// order: new (sorted by file,line: d.go:5 then
	// c.go:30), then still_valid (b.go:20), then
	// resolved (a.go:10). We assert the position by
	// looking for the relative ordering of unique
	// anchors.
	idx := map[string]int{}
	for _, anchor := range []string{
		"New issue.",         // d.go new (line 5)
		"Naming nit.",        // c.go new (line 30)
		"O(n^2) loop.",       // b.go still_valid
		"Hard-coded secret.", // a.go resolved
	} {
		idx[anchor] = strings.Index(out, anchor)
	}
	// The new group sorts alphabetically by file; c.go
	// sorts before d.go regardless of line number.
	if idx["Naming nit."] >= idx["New issue."] {
		t.Errorf("within 'new' group, c.go should appear before d.go (alphabetical file sort); got positions: %+v", idx)
	}
	if idx["New issue."] >= idx["O(n^2) loop."] {
		t.Errorf("'new' group should appear before 'still_valid' group; got positions: %+v", idx)
	}
	if idx["O(n^2) loop."] >= idx["Hard-coded secret."] {
		t.Errorf("'still_valid' group should appear before 'resolved' group; got positions: %+v", idx)
	}
}

// TestBuildSummaryRows_SortOrder pins the sort behaviour
// directly (without the render layer in the way). The
// render-summary test above is end-to-end; this is the
// unit-level pin on the sort key. The sort is:
//  1. Status (new < still_valid < resolved < out_of_scope)
//  2. File (alphabetical)
//  3. Line (numerical)
func TestBuildSummaryRows_SortOrder(t *testing.T) {
	newFindings := []Finding{
		{File: "c.go", Line: 30, Severity: SeverityInfo, Body: "Naming nit."},
		{File: "d.go", Line: 5, Severity: SeverityError, Body: "New issue."},
		// Add a second finding in c.go to pin the
		// "sort by line within the same file" tie-break.
		{File: "c.go", Line: 10, Severity: SeverityWarning, Body: "Earlier c.go finding."},
	}
	carryOver := []PriorFinding{
		{File: "b.go", Line: 20, Body: "O(n^2) loop."},
	}
	resolved := []priorResolvedEntry{
		{Finding: PriorFinding{File: "a.go", Line: 10, Body: "Hard-coded secret."}, Status: StatusResolved, Rationale: "fixed"},
	}
	rows := buildSummaryRows(newFindings, carryOver, resolved)
	// Expected: c.go:10, c.go:30, d.go:5, b.go:20, a.go:10
	wantFiles := []string{"c.go", "c.go", "d.go", "b.go", "a.go"}
	wantLines := []int{10, 30, 5, 20, 10}
	if len(rows) != len(wantFiles) {
		t.Fatalf("got %d rows, want %d", len(rows), len(wantFiles))
	}
	for i := range wantFiles {
		if rows[i].File != wantFiles[i] || rows[i].Line != wantLines[i] {
			t.Errorf("row %d: got %s:%d, want %s:%d", i, rows[i].File, rows[i].Line, wantFiles[i], wantLines[i])
		}
	}
}

// TestRenderSummary_FindingCountInSummary pins the
// "Findings (N)" header in the <summary> block. The
// count must match the total number of rows across all
// status groups — operators use this to spot at a
// glance whether a row was dropped on the floor.
func TestRenderSummary_FindingCountInSummary(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	rows := []SummaryRow{
		{File: "a.go", Line: 1, Severity: "error", Body: "x", Status: StatusNew},
		{File: "b.go", Line: 2, Severity: "warning", Body: "y", Status: StatusStillValid},
		{File: "c.go", Line: 3, Severity: "info", Body: "z", Status: StatusResolved, Rationale: "fixed"},
	}
	out := renderSummary(mr, "", rows, nil)
	if !strings.Contains(out, "<summary>Findings (3)</summary>") {
		t.Errorf("summary count should be 3 (all rows, not just new); got:\n%s", out)
	}
}

// TestRenderSummary_VerdictOverridesSeverity pins the
// emoji-from-Severity behaviour: a finding whose Severity
// is "error" should render with the 🛑 emoji regardless
// of which status group it's in (as long as it's Status
// New). The carried-over / resolved rows use "(prior)"
// instead and don't show the severity emoji.
func TestRenderSummary_VerdictOverridesSeverity(t *testing.T) {
	mr := &gitlab.MergeRequest{SHA: "sha-1"}
	rows := []SummaryRow{
		{File: "x.go", Line: 1, Severity: "error", Category: "x", Body: "x", Status: StatusNew},
	}
	out := renderSummary(mr, "", rows, nil)
	if !strings.Contains(out, "🛑 error") {
		t.Errorf("severity=error should render with 🛑; got:\n%s", out)
	}
}

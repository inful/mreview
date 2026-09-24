package reviewer

import (
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/policy"
)

// TestRenderSummary_EmptyFindings pins the "no issues" path
// so a future regression that prints `## Findings` followed
// by an empty table fails loudly (the "no issues found"
// sentinel is the operator's only signal that mreview ran
// successfully).
func TestRenderSummary_EmptyFindings(t *testing.T) {
	mr := &gitlab.MergeRequest{Title: "Test MR"}
	out := renderSummary(mr, "Looks fine to me.", nil)

	for _, want := range []string{
		"# mreview summary",
		"Looks fine to me.",
		"## Findings",
		"No issues found.",
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
	mr := &gitlab.MergeRequest{}
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
	out := renderSummary(mr, "", findings)

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
	mr := &gitlab.MergeRequest{}
	findings := []policy.EnforcedFinding{
		{File: "a.go", Line: 1, Severity: policy.SeverityError, Category: "security", Body: "Hard-coded secret.", Verdict: policy.SeverityError},
		{File: "b.go", Line: 7, Severity: policy.SeverityWarning, Category: "perf", Body: "O(n^2) loop.", Verdict: policy.SeverityWarning},
		{File: "c.go", Line: 13, Severity: policy.SeverityInfo, Category: "style", Body: "Naming nit.", Verdict: policy.SeverityInfo},
	}
	out := renderSummary(mr, "", findings)

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
	mr := &gitlab.MergeRequest{}
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
	out := renderSummary(mr, "", findings)

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
	mr := &gitlab.MergeRequest{}
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
	out := renderSummary(mr, "", findings)

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
	mr := &gitlab.MergeRequest{}
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
	out := renderSummary(mr, "", findings)

	if !strings.Contains(out, "| 🛑 error |") {
		t.Errorf("verdict (not severity) should drive emoji; got:\n%s", out)
	}
}

// TestRenderSummary_SummaryAndFindings confirms the full
// layout: a top heading, an optional summary paragraph,
// then the table.
func TestRenderSummary_SummaryAndFindings(t *testing.T) {
	mr := &gitlab.MergeRequest{}
	findings := []policy.EnforcedFinding{
		{File: "x.go", Line: 1, Severity: policy.SeverityWarning, Category: "test", Body: "Missing test for foo.", Verdict: policy.SeverityWarning},
	}
	out := renderSummary(mr, "Overall LGTM.", findings)

	wantOrder := []string{
		"# mreview summary",
		"\nOverall LGTM.\n",
		"## Findings",
		"| Severity |",
		"| ⚠️ warning | `x.go` | 1 | test | Missing test for foo. |",
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

package reviewer

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/policy"
)

// fakeRunner is a stand-in Runner for orchestrator tests.
// Records the last call and returns a canned response.
type fakeRunner struct {
	lastSystem string
	lastUser   string
	respText   string
	err        error
	callCount  int
}

func (f *fakeRunner) RunSync(_ context.Context, system, user string) (string, error) {
	f.callCount++
	f.lastSystem = system
	f.lastUser = user
	return f.respText, f.err
}

// TestRunner_Interface pins the Runner contract that the
// orchestrator depends on. New Runner implementations must
// satisfy this interface (compile-time check).
func TestRunner_Interface(t *testing.T) {
	var _ Runner = (*fakeRunner)(nil)
	var _ Runner = (*harnessRunner)(nil)
}

// TestParseReviewResponse_RawJSON covers the happy path: the
// agent emits a clean JSON object that unmarshals directly.
func TestParseReviewResponse_RawJSON(t *testing.T) {
	raw := `{"findings": [{"file": "x.go", "line": 12, "severity": "warning", "category": "test", "body": "missing test", "suggestion": ""}], "summary": "ok"}`
	resp, err := ParseReviewResponse(raw)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if len(resp.Findings) != 1 {
		t.Errorf("got %d findings, want 1", len(resp.Findings))
	}
	if resp.Findings[0].File != "x.go" || resp.Findings[0].Line != 12 {
		t.Errorf("finding = %+v", resp.Findings[0])
	}
	if resp.Summary != "ok" {
		t.Errorf("summary = %q", resp.Summary)
	}
}

// TestParseReviewResponse_Fenced covers the agent emitting
// a ```json ... ``` wrapper. Common with local models.
func TestParseReviewResponse_Fenced(t *testing.T) {
	raw := "```json\n" +
		`{"findings": [], "summary": "lgtm"}` + "\n```"
	resp, err := ParseReviewResponse(raw)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if resp.Summary != "lgtm" {
		t.Errorf("summary = %q, want lgtm", resp.Summary)
	}
}

// TestParseReviewResponse_Empty errors out — the agent
// must emit something.
func TestParseReviewResponse_Empty(t *testing.T) {
	_, err := ParseReviewResponse("")
	if err == nil {
		t.Error("empty response should return an error")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention 'empty', got %v", err)
	}
}

// TestParseReviewResponse_InvalidJSON returns a wrapped
// parse error.
func TestParseReviewResponse_InvalidJSON(t *testing.T) {
	_, err := ParseReviewResponse("not json at all")
	if err == nil {
		t.Error("invalid JSON should return an error")
	}
}

// TestApplyPolicy_NilPolicy_PassesThrough confirms the
// orchestrator's nil-policy handling: agent verdicts stand.
func TestApplyPolicy_NilPolicy_PassesThrough(t *testing.T) {
	o := &Orchestrator{logger: slog.Default()}
	res := o.applyPolicy(policy.Input{
		Findings: []policy.Finding{
			{File: "x.go", Line: 1, Severity: policy.SeverityWarning, Body: "x"},
		},
	})
	if len(res.Findings) != 1 {
		t.Errorf("got %d findings, want 1", len(res.Findings))
	}
	if res.Findings[0].Verdict != policy.SeverityWarning {
		t.Errorf("verdict = %q, want warning (pass-through)", res.Findings[0].Verdict)
	}
	if !res.HasWarning {
		t.Error("HasWarning should be true")
	}
}

// TestApplyPolicy_WithPolicy confirms the enforcer is
// called when a policy is loaded.
func TestApplyPolicy_WithPolicy(t *testing.T) {
	p := &policy.Policy{
		SeverityOverrides: []policy.SeverityOverride{
			{Pattern: "**/*.go", Severity: policy.SeverityError},
		},
	}
	o := &Orchestrator{cfg: Config{Policy: p}, logger: slog.Default()}
	res := o.applyPolicy(policy.Input{
		Findings: []policy.Finding{
			{File: "x.go", Line: 1, Severity: policy.SeverityInfo, Body: "x"},
		},
	})
	if len(res.Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(res.Findings))
	}
	if res.Findings[0].Verdict != policy.SeverityError {
		t.Errorf("verdict = %q, want error (override)", res.Findings[0].Verdict)
	}
	if !res.HasError {
		t.Error("HasError should be true after override to error")
	}
}

// TestOrchestrator_New_RequiresGitLab pins the constructor
// contract.
func TestOrchestrator_New_RequiresGitLab(t *testing.T) {
	_, err := New(Config{
		Runner: &fakeRunner{},
	})
	if err == nil {
		t.Fatal("expected error when GitLab is nil")
	}
	if !strings.Contains(err.Error(), "GitLab is required") {
		t.Errorf("error should mention 'GitLab is required', got %v", err)
	}
}

// TestOrchestrator_New_RequiresRunner pins the constructor
// contract for the Runner.
func TestOrchestrator_New_RequiresRunner(t *testing.T) {
	_, err := New(Config{
		GitLab: &gitlab.Client{},
	})
	if err == nil {
		t.Fatal("expected error when Runner is nil")
	}
	if !strings.Contains(err.Error(), "Runner is required") {
		t.Errorf("error should mention 'Runner is required', got %v", err)
	}
}

// TestOrchestrator_New_AppliesLoggerDefault confirms the
// nil-logger fallback uses slog.Default().
func TestOrchestrator_New_AppliesLoggerDefault(t *testing.T) {
	o, err := New(Config{
		GitLab: &gitlab.Client{},
		Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if o.logger == nil {
		t.Error("logger should be set to slog.Default() when nil")
	}
}

// TestToPolicyFindings verifies the converter between the
// orchestrator's local Finding type and the policy package's
// Finding type. The two have the same field shape but live
// in different packages (mreview doesn't import policy's
// types).
func TestToPolicyFindings(t *testing.T) {
	in := []Finding{
		{File: "a.go", Line: 1, Severity: SeverityWarning, Category: "test", Body: "x"},
	}
	out := toPolicyFindings(in)
	if len(out) != 1 {
		t.Fatalf("got %d, want 1", len(out))
	}
	if out[0].File != "a.go" || out[0].Line != 1 {
		t.Errorf("out = %+v", out[0])
	}
	if out[0].Severity != policy.SeverityWarning {
		t.Errorf("severity = %q, want warning", out[0].Severity)
	}
	if out[0].Category != "test" {
		t.Errorf("category = %q, want test", out[0].Category)
	}
}

// TestChangeFileSource_SatisfiesDiffSource verifies the
// adapter that makes ChangeFile work as a diff.Source. The
// build itself proves this; this test is the documentation
// + a sanity assertion that the methods return the expected
// fields.
func TestChangeFileSource_SatisfiesDiffSource(t *testing.T) {
	cf := &gitlab.ChangeFile{
		OldPath:     "old/a.go",
		NewPath:     "new/a.go",
		NewFile:     true,
		DeletedFile: false,
		RenamedFile: false,
		Diff:        "@@ ... @@\n+x",
	}
	src := changeFileSource{cf: cf}

	if src.Path() != "new/a.go" {
		t.Errorf("Path() = %q, want new/a.go (NewPath for new files)", src.Path())
	}
	if src.Diff() != "@@ ... @@\n+x" {
		t.Errorf("Diff() = %q", src.Diff())
	}
	if !src.IsNew() {
		t.Error("IsNew() should be true")
	}
	if src.IsDeleted() || src.IsRenamed() {
		t.Error("only IsNew should be true")
	}
}

// TestStripFences covers the helper that removes ``` wrappers
// from agent output.
func TestStripFences(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"```json\n{}\n```", "{}"},
		{"```\n{}\n```", "{}"},
		{"no fences here", "no fences here"},
		{"```only open", "```only open"}, // unclosed → pass-through
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := stripFences(c.in)
			if got != c.want {
				t.Errorf("stripFences(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// errIsContextCancelled checks that err wraps context.Canceled
// (used in error-classification tests below).
func errIsContextCancelled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// Reference: keep the helper visible to the linter even if
// unused — PR #4 (orchestrator event streaming) will use it.
var _ = errIsContextCancelled

package policy

import (
	"testing"
)

// makePolicy is a tiny builder so each test row reads as a
// single intent ("with this policy, this finding should
// escalate") instead of a YAML blob.
func makePolicy(overrides []SeverityOverride, labels map[string]Severity) *Policy {
	return &Policy{
		SeverityOverrides: overrides,
		Labels:            labels,
	}
}

// TestEnforce_NilAndEmpty pins the safe-by-default contract:
// nil/empty inputs produce an empty result with no error verdict.
func TestEnforce_NilAndEmpty(t *testing.T) {
	t.Run("nil policy", func(t *testing.T) {
		var p *Policy
		got := p.Enforce(Input{
			Findings: []Finding{{File: "x.go", Severity: SeverityWarning}},
		})
		if got.HasError {
			t.Errorf("nil policy should not escalate; got HasError=true")
		}
		if len(got.Findings) != 1 {
			t.Errorf("got %d findings, want 1 (pass-through)", len(got.Findings))
		}
		if got.Findings[0].Verdict != SeverityWarning {
			t.Errorf("verdict = %q, want warning (pass-through)",
				got.Findings[0].Verdict)
		}
	})

	t.Run("empty findings", func(t *testing.T) {
		p := makePolicy(nil, nil)
		got := p.Enforce(Input{})
		if got.HasError || got.HasWarning || got.HasInfo {
			t.Errorf("empty findings: HasError=%v HasWarning=%v HasInfo=%v",
				got.HasError, got.HasWarning, got.HasInfo)
		}
		if len(got.Findings) != 0 {
			t.Errorf("got %d findings, want 0", len(got.Findings))
		}
	})

	t.Run("no policy rules", func(t *testing.T) {
		p := &Policy{}
		got := p.Enforce(Input{
			Findings: []Finding{
				{File: "x.go", Severity: SeverityWarning},
				{File: "y.go", Severity: SeverityError},
			},
		})
		if got.HasError != true {
			t.Errorf("input had error verdict; got HasError=false")
		}
		// Verdicts preserved.
		if got.Findings[0].Verdict != SeverityWarning {
			t.Errorf("finding 0 verdict = %q, want warning",
				got.Findings[0].Verdict)
		}
		if got.Findings[1].Verdict != SeverityError {
			t.Errorf("finding 1 verdict = %q, want error",
				got.Findings[1].Verdict)
		}
	})
}

// TestEnforce_SeverityOverride pins the matching/escalation
// semantics: first matching override wins; non-matching patterns
// are skipped; weaker severities are not overwritten.
func TestEnforce_SeverityOverride(t *testing.T) {
	p := makePolicy([]SeverityOverride{
		{Pattern: "**/*.go", Severity: SeverityError},
		{Pattern: "internal/security/**", Severity: SeverityError},
	}, nil)

	cases := []struct {
		name        string
		file        string
		severity    Severity
		wantVerdict Severity
		wantReason  string // substring expected in Reason
	}{
		{
			name:        "go file escalated to error",
			file:        "x.go",
			severity:    SeverityWarning,
			wantVerdict: SeverityError,
			wantReason:  "severity_override",
		},
		{
			name:        "go file already error stays error",
			file:        "x.go",
			severity:    SeverityError,
			wantVerdict: SeverityError,
			// Equal severities — the override doesn't change
			// anything, so Reason stays "llm". This is the
			// documented behaviour: stronger() returns false
			// on equal severities.
			wantReason: "llm",
		},
		{
			name:        "go file info still escalated (stronger wins)",
			file:        "x.go",
			severity:    SeverityInfo,
			wantVerdict: SeverityError,
			wantReason:  "severity_override",
		},
		{
			name:        "non-go file passes through",
			file:        "x.md",
			severity:    SeverityWarning,
			wantVerdict: SeverityWarning,
			wantReason:  "llm",
		},
		{
			name:        "security path escalates from info to error",
			file:        "internal/security/secret.go",
			severity:    SeverityInfo,
			wantVerdict: SeverityError,
			wantReason:  "severity_override",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := p.Enforce(Input{
				Findings: []Finding{
					{File: c.file, Severity: c.severity, Body: "test"},
				},
			})
			if len(res.Findings) != 1 {
				t.Fatalf("got %d findings, want 1", len(res.Findings))
			}
			got := res.Findings[0]
			if got.Verdict != c.wantVerdict {
				t.Errorf("Verdict = %q, want %q", got.Verdict, c.wantVerdict)
			}
			if !contains(got.Reason, c.wantReason) {
				t.Errorf("Reason = %q, want substring %q", got.Reason, c.wantReason)
			}
			if c.wantVerdict == SeverityError && !res.HasError {
				t.Errorf("HasError = false, want true (verdict was error)")
			}
		})
	}
}

// TestEnforce_LabelEscalation covers the per-label rule: when
// the MR has a label listed in policy.Labels, every finding
// gets escalated to that severity (strongest wins).
func TestEnforce_LabelEscalation(t *testing.T) {
	p := makePolicy(nil, map[string]Severity{
		"security-review": SeverityError,
		"wip":             SeverityInfo,
	})

	cases := []struct {
		name        string
		labels      []string
		severity    Severity
		wantVerdict Severity
		wantReason  string
	}{
		{
			name:        "no label = pass-through",
			labels:      nil,
			severity:    SeverityWarning,
			wantVerdict: SeverityWarning,
			wantReason:  "llm",
		},
		{
			name:        "unrelated label = pass-through",
			labels:      []string{"documentation"},
			severity:    SeverityWarning,
			wantVerdict: SeverityWarning,
			wantReason:  "llm",
		},
		{
			name:        "security-review label escalates warning to error",
			labels:      []string{"security-review"},
			severity:    SeverityWarning,
			wantVerdict: SeverityError,
			wantReason:  "label:security-review",
		},
		{
			name:        "wip label never escalates",
			labels:      []string{"wip"},
			severity:    SeverityError,
			wantVerdict: SeverityError,
			wantReason:  "llm", // wip=info < error; stronger rule does not downgrade
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := p.Enforce(Input{
				Labels: c.labels,
				Findings: []Finding{
					{File: "x.go", Severity: c.severity, Body: "test"},
				},
			})
			if len(res.Findings) != 1 {
				t.Fatalf("got %d findings", len(res.Findings))
			}
			got := res.Findings[0]
			if got.Verdict != c.wantVerdict {
				t.Errorf("Verdict = %q, want %q", got.Verdict, c.wantVerdict)
			}
			if !contains(got.Reason, c.wantReason) {
				t.Errorf("Reason = %q, want substring %q", got.Reason, c.wantReason)
			}
		})
	}
}

// TestEnforce_LabelAndOverrideBothApply verifies that when
// multiple rules could escalate, the strongest wins and the
// most-recent reason is the strongest-source reason. (We don't
// promise a specific reason format here — only that the
// strongest severity is the verdict.)
func TestEnforce_LabelAndOverrideBothApply(t *testing.T) {
	p := makePolicy(
		[]SeverityOverride{
			{Pattern: "**/*.go", Severity: SeverityWarning},
		},
		map[string]Severity{
			"security-review": SeverityError,
		},
	)
	res := p.Enforce(Input{
		Labels: []string{"security-review"},
		Findings: []Finding{
			{File: "x.go", Severity: SeverityInfo, Body: "x"},
		},
	})
	if len(res.Findings) != 1 {
		t.Fatalf("got %d findings", len(res.Findings))
	}
	if got := res.Findings[0].Verdict; got != SeverityError {
		t.Errorf("Verdict = %q, want error (label rule wins over override)",
			got)
	}
}

// TestEnforce_ForbidRule triggers a synthetic error finding
// when a regex matches file content.
func TestEnforce_ForbidRule(t *testing.T) {
	p := &Policy{
		Forbid: []Forbid{
			{ID: "no-todo", Pattern: "TODO", Message: "no TODO comments"},
		},
	}
	res := p.Enforce(Input{
		Files: []string{"a.go", "b.md"},
		FileContents: map[string]string{
			"a.go": "// TODO: fix this",
			"b.md": "no markers here",
		},
	})
	if !res.HasError {
		t.Errorf("HasError = false; expected synthetic error from forbid match")
	}
	if len(res.Synthetics) != 1 {
		t.Fatalf("got %d synthetics, want 1", len(res.Synthetics))
	}
	got := res.Synthetics[0]
	if got.RuleID != "no-todo" {
		t.Errorf("RuleID = %q, want no-todo", got.RuleID)
	}
	if got.Kind != SyntheticForbid {
		t.Errorf("Kind = %q, want forbid", got.Kind)
	}
	if got.File != "a.go" {
		t.Errorf("File = %q, want a.go (only file with TODO)", got.File)
	}
	if got.Body != "no TODO comments" {
		t.Errorf("Body = %q, want rule message", got.Body)
	}
}

// TestEnforce_ForbidRuleMissed produces no synthetic finding
// when no file content matches.
func TestEnforce_ForbidRuleMissed(t *testing.T) {
	p := &Policy{
		Forbid: []Forbid{
			{ID: "no-todo", Pattern: "TODO", Message: "no TODO"},
		},
	}
	res := p.Enforce(Input{
		Files: []string{"a.go"},
		FileContents: map[string]string{
			"a.go": "package x\n\nfunc X() {}\n",
		},
	})
	if len(res.Synthetics) != 0 {
		t.Errorf("got %d synthetics, want 0 (no match)", len(res.Synthetics))
	}
	if res.HasError {
		t.Errorf("HasError = true; expected no error verdict")
	}
}

// TestEnforce_RequireRuleHit: when the pattern matches at least
// one file, no synthetic finding is produced.
func TestEnforce_RequireRuleHit(t *testing.T) {
	p := &Policy{
		Require: []Require{
			{ID: "has-tests", Pattern: "**/*_test.go", Message: "tests required"},
		},
	}
	res := p.Enforce(Input{
		Files: []string{"foo.go", "foo_test.go"},
	})
	if len(res.Synthetics) != 0 {
		t.Errorf("got %d synthetics, want 0 (pattern matched)", len(res.Synthetics))
	}
	if res.HasError {
		t.Errorf("HasError = true on a satisfied require")
	}
}

// TestEnforce_RequireRuleMissed: when the pattern matches zero
// files, a synthetic error finding is produced.
func TestEnforce_RequireRuleMissed(t *testing.T) {
	p := &Policy{
		Require: []Require{
			{ID: "has-tests", Pattern: "**/*_test.go", Message: "tests required"},
		},
	}
	res := p.Enforce(Input{
		Files: []string{"foo.go"}, // no test file
	})
	if !res.HasError {
		t.Errorf("HasError = false; expected error from require miss")
	}
	if len(res.Synthetics) != 1 {
		t.Fatalf("got %d synthetics, want 1", len(res.Synthetics))
	}
	got := res.Synthetics[0]
	if got.RuleID != "has-tests" {
		t.Errorf("RuleID = %q, want has-tests", got.RuleID)
	}
	if got.Kind != SyntheticRequire {
		t.Errorf("Kind = %q, want require", got.Kind)
	}
	if got.File != "" {
		t.Errorf("File = %q, want empty (require is path-independent)", got.File)
	}
}

// TestEnforce_AllRulesCombined: a single review with findings,
// label escalation, forbid hit, AND require miss. Verifies the
// result carries everything the orchestrator needs.
func TestEnforce_AllRulesCombined(t *testing.T) {
	p := &Policy{
		SeverityOverrides: []SeverityOverride{
			{Pattern: "**/*.go", Severity: SeverityWarning},
		},
		Forbid: []Forbid{
			{ID: "no-todo", Pattern: "TODO", Message: "no TODO"},
		},
		Require: []Require{
			{ID: "has-tests", Pattern: "**/*_test.go", Message: "tests required"},
		},
		Labels: map[string]Severity{
			"security-review": SeverityError,
		},
	}

	res := p.Enforce(Input{
		Files: []string{"foo.go"}, // missing foo_test.go
		FileContents: map[string]string{
			"foo.go": "// TODO\npackage x\n",
		},
		Labels: []string{"security-review"},
		Findings: []Finding{
			{File: "foo.go", Severity: SeverityInfo, Body: "x"},
		},
	})

	// One finding, escalated to error via label rule.
	if len(res.Findings) != 1 {
		t.Errorf("got %d findings, want 1", len(res.Findings))
	} else if res.Findings[0].Verdict != SeverityError {
		t.Errorf("verdict = %q, want error", res.Findings[0].Verdict)
	}

	// Two synthetics: forbid + require miss.
	if len(res.Synthetics) != 2 {
		t.Errorf("got %d synthetics, want 2 (forbid + require)", len(res.Synthetics))
	}
	if !res.HasError {
		t.Errorf("HasError = false; expected error verdict")
	}
}

// TestStronger pins the severity ranking. error > warning > info
// > anything else. Used by Enforce; pinning here so a refactor
// of the rank table doesn't silently change behaviour.
func TestStronger(t *testing.T) {
	cases := []struct {
		a, b Severity
		want bool
	}{
		{SeverityError, SeverityWarning, true},
		{SeverityError, SeverityInfo, true},
		{SeverityError, SeverityError, false}, // equal
		{SeverityWarning, SeverityInfo, true},
		{SeverityWarning, SeverityError, false}, // weaker
		{SeverityWarning, SeverityWarning, false},
		{SeverityInfo, SeverityError, false},
		{SeverityInfo, SeverityWarning, false},
		{SeverityInfo, SeverityInfo, false},
		{"unknown", SeverityInfo, false}, // unknown ranks lowest
	}
	for _, c := range cases {
		t.Run(string(c.a)+"_vs_"+string(c.b), func(t *testing.T) {
			if got := stronger(c.a, c.b); got != c.want {
				t.Errorf("stronger(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// contains is a small string-substring helper. (strings.Contains
// works, but this avoids the import dance in the test file.)
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

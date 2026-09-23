package policy

import (
	"strings"
	"testing"
)

// TestParse_ValidPolicies walks through the schema's surface
// area. Each row pins a different section (severity_overrides,
// forbid, require, labels) and a different combination
// thereof. Adding a new section? Add a row here first.
func TestParse_ValidPolicies(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		// Optional checks run against the parsed Policy.
		check func(t *testing.T, p *Policy)
	}{
		{
			name: "empty policy",
			yaml: ``,
			check: func(t *testing.T, p *Policy) {
				if len(p.SeverityOverrides) != 0 {
					t.Errorf("expected 0 overrides, got %d", len(p.SeverityOverrides))
				}
				if len(p.Forbid) != 0 {
					t.Errorf("expected 0 forbids, got %d", len(p.Forbid))
				}
				if len(p.Require) != 0 {
					t.Errorf("expected 0 requires, got %d", len(p.Require))
				}
				if len(p.Labels) != 0 {
					t.Errorf("expected 0 labels, got %d", len(p.Labels))
				}
			},
		},
		{
			name: "all four sections",
			yaml: `
severity_overrides:
  - pattern: "**/*.go"
    severity: error
forbid:
  - id: no-todo
    pattern: "TODO"
    message: "TODO comments are not allowed"
require:
  - id: has-tests
    pattern: "internal/**/*_test.go"
    message: "internal changes require a test"
labels:
  security-review: error
`,
			check: func(t *testing.T, p *Policy) {
				if len(p.SeverityOverrides) != 1 {
					t.Errorf("overrides = %d, want 1", len(p.SeverityOverrides))
				}
				if len(p.Forbid) != 1 {
					t.Errorf("forbids = %d, want 1", len(p.Forbid))
				}
				if len(p.Require) != 1 {
					t.Errorf("requires = %d, want 1", len(p.Require))
				}
				if len(p.Labels) != 1 {
					t.Errorf("labels = %d, want 1", len(p.Labels))
				}
				if p.Labels["security-review"] != SeverityError {
					t.Errorf("labels[security-review] = %q, want error",
						p.Labels["security-review"])
				}
			},
		},
		{
			name: "multiple overrides",
			yaml: `
severity_overrides:
  - pattern: "**/*.go"
    severity: error
  - pattern: "internal/security/**"
    severity: error
  - pattern: "docs/**"
    severity: info
`,
			check: func(t *testing.T, p *Policy) {
				if len(p.SeverityOverrides) != 3 {
					t.Errorf("overrides = %d, want 3", len(p.SeverityOverrides))
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Parse([]byte(c.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if p == nil {
				t.Fatalf("Parse returned nil policy")
			}
			if c.check != nil {
				c.check(t, p)
			}
		})
	}
}

// TestParse_RejectsInvalid covers every validation branch —
// unknown top-level field, bad severity, missing id, bad regex,
// bad glob. Adding a new validation rule? Add a row here.
func TestParse_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name       string
		yaml       string
		wantErrSub string // substring expected in error
	}{
		{
			name:       "unknown top-level field",
			yaml:       `bogus: 42`,
			wantErrSub: "unknown top-level field",
		},
		{
			name: "bad severity in override",
			yaml: `
severity_overrides:
  - pattern: "**/*.go"
    severity: errror
`,
			wantErrSub: "severity_overrides[0]: severity",
		},
		{
			name: "bad severity in labels",
			yaml: `
labels:
  foo: banana
`,
			wantErrSub: `labels["foo"]: severity`,
		},
		{
			name: "missing forbid id",
			yaml: `
forbid:
  - pattern: "TODO"
    message: "no"
`,
			wantErrSub: "forbid[0]: id is required",
		},
		{
			name: "missing require id",
			yaml: `
require:
  - pattern: "**/*.go"
    message: "x"
`,
			wantErrSub: "require[0]: id is required",
		},
		{
			name: "bad forbid regex",
			yaml: `
forbid:
  - id: broken
    pattern: "[unbalanced"
    message: "x"
`,
			wantErrSub: "forbid[0]",
		},
		{
			name:       "malformed YAML",
			yaml:       `severity_overrides: [this is not valid YAML`,
			wantErrSub: "parse YAML",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil {
				t.Fatalf("Parse accepted invalid YAML: %s", c.yaml)
			}
			if !strings.Contains(err.Error(), c.wantErrSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), c.wantErrSub)
			}
		})
	}
}

// TestValidSeverity pins the recognised severity strings. Adding
// a new severity? Add the constant AND update the switch in
// ValidSeverity AND update severityRank in enforce.go.
func TestValidSeverity(t *testing.T) {
	cases := map[string]bool{
		"info":    true,
		"warning": true,
		"error":   true,
		"":        false,
		"warn":    false,
		"errror":  false,
		"INFO":    false, // case-sensitive
		"Error":   false,
		"debug":   false,
	}
	for s, want := range cases {
		t.Run(s, func(t *testing.T) {
			if got := ValidSeverity(s); got != want {
				t.Errorf("ValidSeverity(%q) = %v, want %v", s, got, want)
			}
		})
	}
}

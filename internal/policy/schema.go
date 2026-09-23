// Package policy loads and enforces a YAML policy file that
// controls how findings from the LLM review are processed.
//
// The schema and exit-code semantics are locked in by issue #42's
// migration step 2 (and the architecture reset's acceptance
// block: "findings flagged `error` per `policy.yaml` fail the
// review with `ExitPolicy = 8`"). Changes to the YAML schema
// are breaking changes for operators — add new fields; don't
// rename or remove existing ones.
//
// The package owns:
//
//   - the YAML schema (schema.go)
//   - the strict loader (policy.go)
//   - the enforcer (enforce.go) — pure function over findings
//
// The orchestrator (PR #3) calls Enforce at the post-merge point
// of the review pipeline: it gets a slice of findings + the MR's
// file list + the MR's labels, and returns the same findings with
// per-finding verdicts attached plus any synthetic findings
// produced by `require` / `forbid` rules.
//
// Reference schema (illustrative — the real one lives in
// schema_test.go's golden cases):
//
//	severity_overrides:
//	  - pattern: "**/*.go"
//	    severity: error        # any finding on .go files becomes error
//	forbid:
//	  - id: "no-todo-comments"
//	    pattern: "TODO"
//	    message: "TODO comments are not allowed"
//	require:
//	  - id: "has-tests"
//	    pattern: "internal/**/*_test.go"
//	    message: "MRs touching internal/ must include a test file"
//	labels:
//	  "security-review":
//	    severity: error        # when MR has the label, escalate
package policy

// Severity is the verdict the enforcer attaches to a finding.
// The zero value is SeverityInfo — anything not explicitly
// escalated is treated as informational.
type Severity string

const (
	// SeverityInfo marks a non-blocking comment (nit,
	// suggestion).
	SeverityInfo Severity = "info"

	// SeverityWarning marks a real issue that should be
	// addressed before merge.
	SeverityWarning Severity = "warning"

	// SeverityError marks a blocker (broken behaviour, policy
	// violation). Findings at this severity make the review
	// exit with `ExitPolicy = 8`.
	SeverityError Severity = "error"
)

// ValidSeverity reports whether s is one of the recognised
// severity strings. Used by the loader to reject typos before
// they reach the enforcer.
func ValidSeverity(s string) bool {
	switch Severity(s) {
	case SeverityInfo, SeverityWarning, SeverityError:
		return true
	default:
		return false
	}
}

// Policy is the parsed + compiled form of the YAML policy file.
// All four top-level sections are optional; a zero-value Policy
// is a valid "no rules" policy.
type Policy struct {
	// SeverityOverrides escalates any finding whose File matches
	// the doublestar glob to the configured severity. Patterns
	// use the doublestar syntax (`**`, `*`, `?`, character
	// classes). The first matching override wins — order
	// matters.
	SeverityOverrides []SeverityOverride

	// Forbid adds a synthetic error finding when the regex
	// matches content in any of the MR's changed files. The
	// regex is matched against the full file content (one big
	// string per file), so anchoring with `(?m)` for
	// line-by-line semantics is the caller's choice.
	Forbid []Forbid

	// Require adds a synthetic error finding when the doublestar
	// glob does NOT match any path in the MR. Use to enforce
	// "if you touched X you must also touch Y" (e.g. internal/
	// requires tests).
	Require []Require

	// Labels maps an MR label name (CI_MERGE_REQUEST_LABELS) to
	// a severity. When the MR carries a label whose key is
	// present, every finding's verdict is escalated to that
	// severity (the strongest escalation wins).
	Labels map[string]Severity
}

// SeverityOverride escalates findings whose File matches the
// doublestar Pattern to the configured Severity.
type SeverityOverride struct {
	Pattern  string
	Severity Severity
}

// Forbid adds a synthetic error finding when Pattern (a Go
// regexp) matches content in any of the MR's changed files.
type Forbid struct {
	ID      string
	Pattern string
	Message string
}

// Require adds a synthetic error finding when the doublestar
// Pattern matches zero files in the MR.
type Require struct {
	ID      string
	Pattern string
	Message string
}

// EnforcedFinding is a Finding with a verdict attached. The
// enforcer produces these; the orchestrator (PR #3) renders them
// back into the GitLab payload with appropriate severity
// decorations.
type EnforcedFinding struct {
	File     string
	Line     int
	Severity Severity
	Category string
	Body     string

	// Verdict is the final severity after all policy rules
	// have been applied. Equals the original Severity when no
	// rule escalates it.
	Verdict Severity

	// Reason explains how the verdict was assigned (for
	// rendering in the GitLab comment and for debugging).
	Reason string
}

// SyntheticFinding is a finding produced by the enforcer itself
// (from a `forbid` match or a missing `require` rule). The
// orchestrator renders these as comments too — they're
// indistinguishable from LLM findings at the GitLab layer.
type SyntheticFinding struct {
	File   string // "" when the rule is path-independent
	Line   int    // 0 when not applicable
	Body   string
	RuleID string // the Forbid.ID or Require.ID that fired
	Kind   SyntheticKind
}

// SyntheticKind identifies the source of a synthetic finding.
type SyntheticKind string

const (
	// SyntheticForbid marks a finding produced by a `forbid`
	// rule that matched content.
	SyntheticForbid SyntheticKind = "forbid"

	// SyntheticRequire marks a finding produced by a `require`
	// rule whose pattern matched zero files.
	SyntheticRequire SyntheticKind = "require"
)

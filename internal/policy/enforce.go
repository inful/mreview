package policy

import (
	"regexp"

	"github.com/bmatcuk/doublestar/v4"
)

// Finding is the shape the enforcer operates on. The
// orchestrator (PR #3) converts internal/llm.Finding into this
// shape via a one-liner adapter; the policy package owns its
// own type so it doesn't import internal/llm (which PR #3 will
// delete).
type Finding struct {
	File     string
	Line     int
	Severity Severity
	Category string
	Body     string
}

// Input bundles the data Enforce needs. Files is the list of
// paths in the MR; FileContents is a map of path → file content
// (used by `forbid` rules); Labels is the MR's labels
// (CI_MERGE_REQUEST_LABELS).
type Input struct {
	Findings     []Finding
	Files        []string
	FileContents map[string]string
	Labels       []string
}

// Result is the enforcer's output. Findings carries the original
// findings with per-finding verdicts attached (and Verdict
// populated); Synthetics carries the rules-generated findings
// (one per `forbid` hit or `require` miss). HasError reports
// whether any verdict (in Findings or Synthetics) is at
// SeverityError — the orchestrator uses that to decide whether
// to fail the review with ExitPolicy.
type Result struct {
	Findings   []EnforcedFinding
	Synthetics []SyntheticFinding
	HasError   bool
	HasWarning bool
	HasInfo    bool
}

// Enforce evaluates findings against the policy and returns the
// annotated result. Nil-safe: nil findings → empty result;
// nil policy → pass-through with default SeverityInfo verdicts.
//
// The enforcer is a pure function — no I/O, no logging, no
// clock. Tests assert every rule combination without
// stubbing.
func (p *Policy) Enforce(in Input) Result {
	if p == nil {
		// nil policy = no rules; pass-through. The method
		// receiver would dereference nil on the first field
		// access below, so handle this case first.
		res := Result{}
		for _, f := range in.Findings {
			res.Findings = append(res.Findings, EnforcedFinding{
				File:     f.File,
				Line:     f.Line,
				Severity: f.Severity,
				Category: f.Category,
				Body:     f.Body,
				Verdict:  f.Severity,
				Reason:   "llm",
			})
			recordVerdict(&res, f.Severity)
		}
		return res
	}

	res := Result{}

	for _, f := range in.Findings {
		ef := EnforcedFinding{
			File:     f.File,
			Line:     f.Line,
			Severity: f.Severity,
			Category: f.Category,
			Body:     f.Body,
			Verdict:  f.Severity,
			Reason:   "llm",
		}

		// Apply severity_overrides (first match wins).
		for _, ov := range p.SeverityOverrides {
			if pathMatches(ov.Pattern, f.File) {
				if stronger(ov.Severity, ef.Verdict) {
					ef.Verdict = ov.Severity
					ef.Reason = "severity_override:" + ov.Pattern
				}
				break
			}
		}

		// Apply label-based escalation.
		for _, label := range in.Labels {
			if sev, ok := p.Labels[label]; ok {
				if stronger(sev, ef.Verdict) {
					ef.Verdict = sev
					ef.Reason = "label:" + label
				}
			}
		}

		res.Findings = append(res.Findings, ef)
		recordVerdict(&res, ef.Verdict)
	}

	// Apply forbid rules — regex match against file content.
	for _, fb := range p.Forbid {
		re := regexp.MustCompile(fb.Pattern) // already validated at Load()
		for path, content := range in.FileContents {
			if re.MatchString(content) {
				res.Synthetics = append(res.Synthetics, SyntheticFinding{
					File:   path,
					Line:   0,
					Body:   fb.Message,
					RuleID: fb.ID,
					Kind:   SyntheticForbid,
				})
				recordVerdict(&res, SeverityError)
			}
		}
	}

	// Apply require rules — fail when no file matches.
	for _, rq := range p.Require {
		matched := false
		for _, f := range in.Files {
			if pathMatches(rq.Pattern, f) {
				matched = true
				break
			}
		}
		if !matched {
			res.Synthetics = append(res.Synthetics, SyntheticFinding{
				File:   "",
				Line:   0,
				Body:   rq.Message,
				RuleID: rq.ID,
				Kind:   SyntheticRequire,
			})
			recordVerdict(&res, SeverityError)
		}
	}

	return res
}

// pathMatches reports whether path matches the doublestar glob.
// Doublestar's Match returns an error on invalid patterns; we
// already validated at Load time, so a non-nil error here means
// a runtime-only failure (e.g. a malformed path) — treat as
// non-match.
func pathMatches(pattern, path string) bool {
	ok, err := doublestar.Match(pattern, path)
	if err != nil {
		return false
	}
	return ok
}

// stronger returns true when a's severity is at least as strong
// as b's. Strongness ordering: error > warning > info. When
// they're equal, stronger returns false (we don't overwrite
// equal-strength rules — the first matching rule wins).
func stronger(a, b Severity) bool {
	return severityRank(a) > severityRank(b)
}

func severityRank(s Severity) int {
	switch s {
	case SeverityError:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	default:
		return 0
	}
}

// recordVerdict bumps the Result's flags based on the verdict.
// Centralised so future severity levels (e.g. SeverityCritical)
// only need to be added in one place.
func recordVerdict(r *Result, s Severity) {
	switch s {
	case SeverityError:
		r.HasError = true
	case SeverityWarning:
		r.HasWarning = true
	case SeverityInfo:
		r.HasInfo = true
	}
}

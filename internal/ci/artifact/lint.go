package artifact

// LintResult summarises `golangci-lint run --out-format=json`.
// The raw format is a JSON array of issue objects; we keep
// the count + a slice of the highest-severity ones so the
// prompt doesn't bloat on a huge lint result.
type LintResult struct {
	Total    int
	Errors   int
	Warnings int
	Issues   []LintIssue // capped (see LintIssueCap)
}

// LintIssue is one entry from golangci-lint's JSON output.
// Fields cover the parts the agent needs to identify the
// issue and reason about severity.
type LintIssue struct {
	FromLinter string `json:"FromLinter"`
	Text       string `json:"Text"`
	Severity   string `json:"Severity"` // "error" / "warning" / ...
	SourceLine string `json:"SourceLine"`
	Pos        struct {
		Filename string `json:"Filename"`
		Line     int    `json:"Line"`
	} `json:"Pos"`
}

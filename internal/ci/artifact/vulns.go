package artifact

// VulnResult summarises `govulncheck` output. The raw format
// is a JSON document with a top-level "vulnerabilities" array;
// we keep the count + a slice of the highest-severity ones.
type VulnResult struct {
	Total    int
	Findings []VulnFinding
}

// VulnFinding is one entry from govulncheck's JSON output.
// Fields cover the parts the agent needs to identify the
// vulnerability and reason about severity.
type VulnFinding struct {
	ID       string `json:"ID"` // e.g. "GO-2023-1234"
	Summary  string `json:"Summary"`
	Details  string `json:"Details"`
	Package  string `json:"Package"` // module path
	Version  string `json:"Version"`
	Severity string `json:"Severity"` // govulncheck-specific labels
}

package artifact

import (
	"encoding/json"
	"strings"
)

// LoadLint parses `golangci-lint run --out-format=json`. The
// raw format is a JSON array of issue objects; we count
// severities and keep a capped slice of issues for the
// prompt renderer.
//
// Missing / empty / unreadable → NotAvailable. Malformed
// (not an array, or first element fails to parse) →
// ParseError set.
func LoadLint(path string) LoadResult[LintResult] {
	res := LoadResult[LintResult]{Path: path}
	data, size, err := readFile(path)
	if err != nil {
		res.NotAvailable = true
		res.SizeBytes = size
		return res
	}
	res.SizeBytes = size

	var issues []LintIssue
	if err := json.Unmarshal(data, &issues); err != nil {
		res.ParseError = err
		return res
	}

	lr := LintResult{Total: len(issues)}
	for _, iss := range issues {
		switch strings.ToLower(iss.Severity) {
		case "error":
			lr.Errors++
		case "warning":
			lr.Warnings++
		}
	}
	// Keep a capped sample — the agent reads counts first
	// and details on demand.
	if len(issues) > LintIssueCap {
		issues = issues[:LintIssueCap]
	}
	lr.Issues = issues
	res.Value = &lr
	return res
}

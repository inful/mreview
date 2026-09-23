package artifact

import (
	"encoding/json"
)

// VulnFindingCap caps the number of findings the vuln loader
// keeps. govulncheck outputs can be small (a handful of
// CVEs) — the cap is a safety net for repos with hundreds of
// outdated deps.
const VulnFindingCap = 50

// LoadVulns parses `govulncheck` output. The raw format is a
// JSON document with a top-level "findings" array (newer
// govulncheck) or "vulnerabilities" array (older). We
// attempt the newer shape first; fall back to the older one
// when the newer is empty.
//
// Missing / empty / unreadable → NotAvailable. Malformed →
// ParseError set.
func LoadVulns(path string) LoadResult[VulnResult] {
	res := LoadResult[VulnResult]{Path: path}
	data, size, err := readFile(path)
	if err != nil {
		res.NotAvailable = true
		res.SizeBytes = size
		return res
	}
	res.SizeBytes = size

	// Try the newer "findings" shape first.
	var newer struct {
		Findings []VulnFinding `json:"findings"`
	}
	if err := json.Unmarshal(data, &newer); err == nil && len(newer.Findings) > 0 {
		res.Value = vulnsFromFindings(newer.Findings)
		return res
	}

	// Fall back to the older "vulnerabilities" shape.
	var older struct {
		Vulnerabilities []VulnFinding `json:"vulnerabilities"`
	}
	if err := json.Unmarshal(data, &older); err != nil {
		res.ParseError = err
		return res
	}
	res.Value = vulnsFromFindings(older.Vulnerabilities)
	return res
}

// vulnsFromFindings builds a VulnResult from a slice of
// findings, applying the VulnFindingCap.
func vulnsFromFindings(findings []VulnFinding) *VulnResult {
	if len(findings) > VulnFindingCap {
		findings = findings[:VulnFindingCap]
	}
	return &VulnResult{
		Total:    len(findings),
		Findings: findings,
	}
}

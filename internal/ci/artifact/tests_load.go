package artifact

import (
	"bufio"
	"bytes"
	"encoding/json"
)

// LoadTests parses `go test -json ./...` output. The format
// is newline-delimited JSON; one object per test event
// (run / pass / fail / skip / output / ...).
//
// Missing / empty / unreadable → NotAvailable. Malformed
// → ParseError set, Value stays nil (the agent doesn't
// see any test results — degraded review).
func LoadTests(path string) LoadResult[TestResult] {
	res := LoadResult[TestResult]{Path: path}
	data, size, err := readFile(path)
	if err != nil {
		res.NotAvailable = true
		res.SizeBytes = size
		return res
	}
	res.SizeBytes = size

	tr := TestResult{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ev struct {
			Action  string `json:"Action"`
			Test    string `json:"Test"`
			Package string `json:"Package"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			// One bad line shouldn't poison the whole
			// parse. Skip it; if the count is wildly off
			// vs. the agent's expectation, it can dig in.
			continue
		}
		tr.Total++
		switch ev.Action {
		case "pass":
			tr.Pass++
		case "fail":
			tr.Fail++
			tr.Failures = append(tr.Failures, TestFailure{
				Test:    ev.Test,
				Package: ev.Package,
				Action:  ev.Action,
			})
		case "skip":
			tr.Skip++
		}
	}
	if err := scanner.Err(); err != nil {
		// Genuine read error mid-stream (not malformed
		// JSON, which we tolerate). Treat as malformed.
		res.ParseError = err
		res.SizeBytes = size
		return res
	}
	res.Value = &tr
	return res
}

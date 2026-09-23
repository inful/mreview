package artifact

// TestResult summarises the `go test -json ./...` output.
// The raw format is newline-delimited JSON (NDJSON), one
// object per test event. We parse every line and aggregate
// pass/fail/skip counts + the failing test names.
type TestResult struct {
	Pass     int
	Fail     int
	Skip     int
	Total    int
	Failures []TestFailure
}

// TestFailure carries the failing test name + the package
// path. The agent uses this to reason about whether a code
// change broke a previously-passing test.
type TestFailure struct {
	Test    string `json:"Test"`
	Package string `json:"Package"`
	Action  string `json:"Action"`
}

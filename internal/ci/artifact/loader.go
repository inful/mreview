// Package artifact loads the CI artifacts that the central
// CI pipeline emits before mreview runs:
//
//   - build.log          (plain text from `go build ./...`)
//   - test_results.json  (NDJSON from `go test -json -count=1 ./...`)
//   - lint.json          (JSON array from `golangci-lint run --out-format=json`)
//   - vulns.json         (JSON from `govulncheck ./...`)
//
// Per the architecture reset (#42) and its step 5 (#43), the
// harness agent never re-runs these tools — CI did it once,
// mreview reads the artifacts and injects them into the
// harness runtime's context. The agent sees them at startup
// (rendered into the system prompt by internal/prompts) and
// can reason about them when producing findings.
//
// **Robustness is the headline requirement.** Missing /
// malformed / empty artifacts must cause DEGRADED information,
// not errors. The agent sees an explicit "this artifact was
// not available" marker and self-calibrates its confidence.
//
//   - missing  → LoadResult.NotAvailable = true
//   - empty    → LoadResult.NotAvailable = true (treated as missing)
//   - malformed→ LoadResult.ParseError set; Value nil
//   - unreadable→ LoadResult.NotAvailable = true
//
// LoadAll itself returns an error ONLY for catastrophic
// failures (the directory itself is unreachable). Per-artifact
// failures live in LoadResult so the prompt can render them
// and the agent can decide what to do.
package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// LoadResult is the outcome of one artifact load. Exactly
// one of Value and NotAvailable is set when the file is
// present-and-valid; ParseError is set when the file is
// present but corrupt (Value stays nil).
type LoadResult[T any] struct {
	// Value is the parsed artifact. Nil when the file is
	// missing, empty, unreadable, or malformed.
	Value *T

	// NotAvailable is true when the file is missing, empty
	// (zero bytes), or unreadable due to permissions. Treated
	// as "degraded" by the prompt renderer — the agent sees
	// an explicit "NOT AVAILABLE" marker.
	NotAvailable bool

	// ParseError is set when the file is present but doesn't
	// parse. Value stays nil. The prompt renderer surfaces
	// the raw content with a "malformed" marker so the agent
	// can still reason about it.
	ParseError error

	// Path is the absolute path that was attempted. Echoed
	// in the prompt so operators can debug "where did you
	// look?" questions.
	Path string

	// SizeBytes is the file size when read. Zero when
	// NotAvailable. Used by the prompt renderer for the
	// "size hint" line.
	SizeBytes int64
}

// Source is the input bundle for LoadAll. Mirrors the
// artifacts the central CI YAML is expected to produce.
// Each field's path is relative to the artifacts-dir; LoadAll
// resolves them against dir.
type Source struct {
	BuildPath string
	TestsPath string
	LintPath  string
	VulnsPath string
}

// Set is the aggregated result. Each field is the
// LoadResult for one artifact. The orchestrator threads the
// whole set into the prompt template.
//
// Renamed from ArtifactSet to Set per the Go naming
// convention (stuttering: artifact.ArtifactSet is the
// canonical Go complaint).
type Set struct {
	Build LoadResult[BuildResult]
	Tests LoadResult[TestResult]
	Lint  LoadResult[LintResult]
	Vulns LoadResult[VulnResult]

	// SourceDir is the absolute path of the directory we
	// loaded from. Surfaced in the prompt for traceability.
	SourceDir string
}

// LoadAll reads every artifact in dir per source. Missing /
// malformed / empty artifacts are recorded as NotAvailable /
// ParseError respectively; LoadAll itself only returns an
// error when dir itself is unreachable (catastrophic —
// every artifact is unavailable by definition).
//
// Per the architecture reset's acceptance criteria:
//   - missing artifact   → degraded info, no error
//   - malformed artifact → degraded info, no error
//   - empty artifact     → degraded info, no error
//   - unreadable artifact→ degraded info, no error
//   - directory unreachable → ExitConfig before any review
func LoadAll(dir string, src Source) (Set, error) {
	// Verify the directory itself. Per-artifact failures are
	// NOT errors at this layer — they live in LoadResult.
	//
	// Stat returns an error in two cases:
	//   1. the path doesn't exist (catastrophic for LoadAll)
	//   2. permission denied (catastrophic)
	// Either way, every artifact is unavailable; the caller
	// should treat this as a fatal config error and exit.
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Set{}, fmt.Errorf("artifact: directory %q does not exist", dir)
		}
		return Set{}, fmt.Errorf("artifact: stat %q: %w", dir, err)
	}

	set := Set{SourceDir: dir}
	set.Build = LoadBuild(joinPath(dir, src.BuildPath))
	set.Tests = LoadTests(joinPath(dir, src.TestsPath))
	set.Lint = LoadLint(joinPath(dir, src.LintPath))
	set.Vulns = LoadVulns(joinPath(dir, src.VulnsPath))
	return set, nil
}

// joinPath is a tiny path-joiner that handles the empty-string
// case (Source.X is "" when the artifact isn't configured —
// e.g. an operator skips the vulns stage). The loader treats
// "" as "no artifact configured" → NotAvailable.
func joinPath(dir, file string) string {
	if file == "" {
		return ""
	}
	if dir == "" {
		return file
	}
	return dir + "/" + file
}

// readFile is the read helper shared by every loader. Returns
// the content + size on success; a "missing / empty /
// unreadable" sentinel otherwise. Loaders wrap this in a
// typed LoadResult.
func readFile(path string) ([]byte, int64, error) {
	if path == "" {
		// Empty path = artifact not configured. Treat as
		// missing — the prompt will render "NOT AVAILABLE".
		return nil, 0, errMissing
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	const maxBytes = 16 * 1024 * 1024 // 16 MiB per artifact — generous upper bound
	const maxRead = 1 * 1024 * 1024   // 1 MiB kept inline; the rest is truncated
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return nil, 0, err
	}
	if len(data) == 0 {
		return nil, 0, errMissing
	}
	if int64(len(data)) > int64(maxRead) {
		// Truncate the in-memory copy. The agent sees a
		// truncation marker in the prompt. Future enhancement:
		// spill to disk.
		truncNote := "\n\n[artifact truncated: kept first 1 MiB of " +
			fmtInt(int64(len(data))) + " bytes]\n"
		data = append(data[:maxRead], []byte(truncNote)...)
	}
	return data, int64(len(data)), nil
}

// fmtInt formats an int64 without pulling strconv.
func fmtInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// errMissing is the sentinel for "file not present / empty
// / unreadable". Treated as NotAvailable by the loaders.
var errMissing = errors.New("artifact: missing")

// jsonUnmarshal is a thin wrapper so the loaders don't all
// duplicate the same boilerplate. Kept available so future
// loaders can share it without re-importing json.
var _ = json.Unmarshal

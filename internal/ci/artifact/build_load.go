package artifact

import (
	"bufio"
	"bytes"
	"strings"
)

// LoadBuild parses the `go build ./...` log. Returns a
// LoadResult with BuildResult populated when the file is
// present and readable.
//
// The file is plain text — we keep the last 50 lines and a
// flag indicating whether the build log shows errors. No
// full AST parsing (build output is intentionally free-form).
//
// Missing / empty / unreadable → NotAvailable=true; the
// prompt surfaces an explicit "NOT AVAILABLE" marker.
func LoadBuild(path string) LoadResult[BuildResult] {
	res := LoadResult[BuildResult]{Path: path}
	data, size, err := readFile(path)
	if err != nil {
		// errMissing from readFile → file is missing, empty,
		// or unreadable. All three surface the same way to
		// the agent.
		res.NotAvailable = true
		res.SizeBytes = size
		return res
	}
	res.SizeBytes = size

	// Detect build-error patterns. We scan all lines (not
	// just the tail) so a build break earlier in the log is
	// still flagged even if the trailing lines look clean.
	var buildErr bool
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "error:") || strings.Contains(line, "undefined:") {
			buildErr = true
			break
		}
	}

	// Keep the last N lines.
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > LastLineCount {
		lines = lines[len(lines)-LastLineCount:]
	}

	res.Value = &BuildResult{
		LastLines:  string(bytes.Join(lines, []byte("\n"))),
		BuildError: buildErr,
	}
	return res
}

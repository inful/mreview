package llm

import (
	"fmt"
	"strings"

	"github.com/inful/mreview/internal/gitlab"
)

// Chunk is one per-file (or per-file-part) unit of a diff sent to
// the LLM. Each chunk maps 1:1 to one prompt slot in the user
// message; the reviewer loops over chunks and calls the LLM once
// per chunk (or merged across chunks in phase 5's "merge verdict"
// pass).
type Chunk struct {
	// File is the canonical path the LLM should reference in any
	// finding. NewPath for new/modified files; OldPath for
	// deleted files; NewPath for renames (matches what GitLab
	// shows in the file tree at HEAD).
	File string

	// OldPath / NewPath are preserved so the caller can emit
	// GitLab discussion positions that match either side.
	OldPath   string
	NewPath   string
	IsNew     bool
	IsDeleted bool
	IsRenamed bool

	// Part / TotalParts describe sub-chunking. Part == 0 means
	// the whole file fit in one chunk; Part > 0 means the file
	// was split into TotalParts pieces at hunk boundaries.
	Part       int
	TotalParts int

	// Diff is the raw unified-diff text (or the portion of it
	// assigned to this chunk).
	Diff string

	// Size is len(Diff); carried so callers can sum budget usage
	// without re-measuring.
	Size int
}

// OversizedError is returned by ChunkByFile when a single file
// can't fit even after splitting. The operator should split the
// MR (or raise maxBytes) — mreview refuses to silently truncate.
type OversizedError struct {
	File     string
	Size     int
	MaxBytes int
}

func (e *OversizedError) Error() string {
	return fmt.Sprintf(
		"llm: file %q is %d bytes, exceeds maxBytes %d even after hunk-level splitting; split the MR manually or raise the per-file budget",
		e.File, e.Size, e.MaxBytes,
	)
}

// ChunkByFile converts GitLab diffs into per-file chunks under the
// given per-chunk byte budget. Files that fit become one chunk
// each; files that overflow are split at hunk boundaries (the
// `@@ ... @@` lines that unified diff uses). A file that overflows
// the budget even after splitting returns *OversizedError so the
// caller can surface it as ExitConfig.
//
// Binary files (Diff == "") are skipped — the LLM has nothing to
// review.
//
// maxBytes is the per-chunk budget (a rough proxy for tokens:
// len/4 is the conventional heuristic). Zero or negative values
// return an error.
func ChunkByFile(changes []gitlab.ChangeFile, maxBytes int) ([]Chunk, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("llm: maxBytes must be > 0, got %d", maxBytes)
	}

	var chunks []Chunk
	for _, c := range changes {
		// Skip binary / no-diff entries (renamed-with-no-change,
		// binary files, etc.).
		if c.Diff == "" {
			continue
		}
		// GitLab sometimes wraps the diff in "diff --git a/... b/..."
		// headers that we want to preserve with the chunk.
		wrapped := wrapDiffWithHeader(c)
		size := len(wrapped)

		if size <= maxBytes {
			chunks = append(chunks, Chunk{
				File:       c.Path(),
				OldPath:    c.OldPath,
				NewPath:    c.NewPath,
				IsNew:      c.NewFile,
				IsDeleted:  c.DeletedFile,
				IsRenamed:  c.RenamedFile,
				Part:       0,
				TotalParts: 0,
				Diff:       wrapped,
				Size:       size,
			})
			continue
		}

		// Overflow: split at hunk boundaries.
		parts := splitDiffAtHunks(wrapped, maxBytes)
		// If after splitting we still have a part larger than the
		// budget (meaning even the smallest hunk doesn't fit), the
		// file can't be reviewed at this budget — surface
		// OversizedError so the operator splits the MR manually.
		for _, p := range parts {
			if len(p) > maxBytes {
				return nil, &OversizedError{
					File:     c.Path(),
					Size:     size,
					MaxBytes: maxBytes,
				}
			}
		}
		for i, part := range parts {
			chunks = append(chunks, Chunk{
				File:       c.Path(),
				OldPath:    c.OldPath,
				NewPath:    c.NewPath,
				IsNew:      c.NewFile,
				IsDeleted:  c.DeletedFile,
				IsRenamed:  c.RenamedFile,
				Part:       i + 1,
				TotalParts: len(parts),
				Diff:       part,
				Size:       len(part),
			})
		}
	}
	return chunks, nil
}

// wrapDiffWithHeader prefixes a unified-diff body with the standard
// "diff --git a/old b/new\n" header so a chunk split off from the
// middle of a multi-file diff still parses as a valid hunk. The
// LLM doesn't strictly need this, but the prompt reads more
// naturally when each chunk looks like a small MR.
func wrapDiffWithHeader(c gitlab.ChangeFile) string {
	var b strings.Builder
	old := c.OldPath
	if old == "" {
		old = c.NewPath
	}
	newPath := c.NewPath
	if newPath == "" {
		newPath = c.OldPath
	}
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n", old, newPath)
	if c.NewFile {
		fmt.Fprintf(&b, "new file mode 100644\n")
	}
	if c.DeletedFile {
		fmt.Fprintf(&b, "deleted file mode 100644\n")
	}
	if c.RenamedFile {
		fmt.Fprintf(&b, "rename from %s\nrename to %s\n", old, newPath)
	}
	b.WriteString(c.Diff)
	return b.String()
}

// splitDiffAtHunks splits a unified-diff body into pieces each ≤
// maxBytes, preferring to break at `@@ ... @@` lines. Falls back to
// line-level splitting within a single hunk when even one hunk
// exceeds the budget — that case is the caller's responsibility
// (it becomes OversizedError).
//
// Algorithm:
//
//  1. Find every hunk-header line and compute its [start, end)
//     range (end is the next hunk header or the end of lines).
//  2. Greedily accumulate hunks into chunks; flush `current` when
//     adding the next hunk would overflow.
//  3. If any hunk alone exceeds maxBytes, fall back to lineSplit
//     for that hunk and continue with the rest.
func splitDiffAtHunks(diff string, maxBytes int) []string {
	lines := strings.Split(diff, "\n")
	hunkRanges := hunkRanges(lines)

	var parts []string
	current := make([]string, 0, len(lines))
	currentSize := 0

	for _, r := range hunkRanges {
		hunkLines := lines[r.start:r.end]
		hunkSize := totalSize(hunkLines)

		// Single hunk too big: flush current, line-split this
		// hunk, then continue from after it.
		if hunkSize > maxBytes {
			if currentSize > 0 {
				parts = append(parts, strings.Join(current, "\n"))
				current = nil
				currentSize = 0
			}
			parts = append(parts, lineSplit(hunkLines, maxBytes)...)
			continue
		}

		// Would adding this hunk overflow? Flush current.
		if currentSize+hunkSize > maxBytes && len(current) > 0 {
			parts = append(parts, strings.Join(current, "\n"))
			current = nil
			currentSize = 0
		}
		current = append(current, hunkLines...)
		currentSize += hunkSize
	}
	if len(current) > 0 {
		parts = append(parts, strings.Join(current, "\n"))
	}
	return parts
}

// hunkRange describes a single hunk's lines [start, end).
type hunkRange struct {
	start, end int
}

// hunkRanges returns the line ranges that cover each hunk in
// lines. Each hunk is `[start, end)` where `start` is the @@ line
// index and `end` is the next hunk's @@ line (or len(lines)).
//
// Hunks that appear outside the standard unified-diff format (e.g.
// the "diff --git" / "index" / "---" / "+++" headers GitLab
// prepends) are NOT hunks and are returned as their own
// "preamble" range before the first hunk.
func hunkRanges(lines []string) []hunkRange {
	var ranges []hunkRange
	preambleEnd := len(lines)
	for i, line := range lines {
		if strings.HasPrefix(line, "@@") && strings.HasSuffix(line, "@@") {
			preambleEnd = i
			break
		}
	}
	if preambleEnd > 0 {
		ranges = append(ranges, hunkRange{start: 0, end: preambleEnd})
	}
	hunkStarts := []int{}
	for i, line := range lines {
		if strings.HasPrefix(line, "@@") && strings.HasSuffix(line, "@@") {
			hunkStarts = append(hunkStarts, i)
		}
	}
	for i, start := range hunkStarts {
		end := len(lines)
		if i+1 < len(hunkStarts) {
			end = hunkStarts[i+1]
		}
		ranges = append(ranges, hunkRange{start: start, end: end})
	}
	return ranges
}

// lineSplit splits lines into chunks each ≤ maxBytes, breaking at
// line boundaries (used as the fallback when a single hunk is too
// large).
func lineSplit(lines []string, maxBytes int) []string {
	var parts []string
	current := make([]string, 0, len(lines))
	var size int
	for _, line := range lines {
		lineSize := len(line) + 1
		if size+lineSize > maxBytes && len(current) > 0 {
			parts = append(parts, strings.Join(current, "\n"))
			current = nil
			size = 0
		}
		current = append(current, line)
		size += lineSize
	}
	if len(current) > 0 {
		parts = append(parts, strings.Join(current, "\n"))
	}
	return parts
}

// totalSize sums len(line)+1 across lines (the +1 stands in for
// the newline that split() removes).
func totalSize(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

// EstimateTokens returns a rough character/4 token estimate for
// the joined chunks. It's intentionally cheap — a real tokenizer
// would model the model's vocabulary; this is good enough for
// budget guard rails.
//
// Uses len(Diff) rather than Chunk.Size so the estimate works
// even when callers construct Chunk literals by hand (testing
// helpers, prompt previews, etc.).
func EstimateTokens(chunks []Chunk) int {
	total := 0
	for _, c := range chunks {
		total += len(c.Diff)
	}
	return total / 4
}

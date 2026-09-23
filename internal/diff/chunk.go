// Package diff splits a code change into per-file chunks sized
// for the LLM's context budget. The chunker is intentionally
// language-agnostic — it operates on Source (a small interface
// the Gitlab / harness / tests adapt into).
//
// The chunker is the one surviving piece of the old
// internal/llm/ package after migration step 3 of the
// architecture reset (#42). It moved here because
//
//   - it doesn't belong in the LLM package (harness owns the
//     LLM loop now);
//   - it doesn't belong in the GitLab package (the package
//     owns GitLab plumbing now);
//   - and tests for it shouldn't have to import either.
//
// Source decouples callers from the Gitlab ChangeFile type
// (closes issue #12 inline).
package diff

// Source is the minimal interface the chunker needs from a
// changed file. GitLab's ChangeFile, the harness's file tools,
// and test fixtures all satisfy this implicitly.
type Source interface {
	// Diff returns the unified-diff body. Empty means "skip"
	// (binary file, rename-with-no-change, etc.).
	Diff() string

	// OldPath / NewPath let the chunker build a synthetic
	// "diff --git" header so a chunk split off from a multi-
	// file diff still parses as a valid hunk.
	OldPath() string
	NewPath() string

	// IsNew / IsDeleted / IsRenamed carry status flags the
	// prompt uses to label the chunk. Renamed + (OldPath !=
	// NewPath) makes the chunker emit a `rename from / rename
	// to` header.
	IsNew() bool
	IsDeleted() bool
	IsRenamed() bool

	// Path is the canonical path the LLM should reference in
	// any finding. NewPath for new / modified files; OldPath
	// for deleted files; NewPath for renames.
	Path() string
}

// Chunk is one per-file (or per-file-part) unit of a diff sent
// to the LLM.
type Chunk struct {
	File       string
	OldPath    string
	NewPath    string
	IsNew      bool
	IsDeleted  bool
	IsRenamed  bool
	Part       int // 1-indexed; 0 means the whole file fit
	TotalParts int
	Diff       string
	Size       int
}

// OversizedError is returned by ChunkByFile when a single file
// can't fit even after splitting.
type OversizedError struct {
	File     string
	Size     int
	MaxBytes int
}

func (e *OversizedError) Error() string {
	return "diff: file " + e.File + " is " +
		itoa(e.Size) + " bytes, exceeds maxBytes " +
		itoa(e.MaxBytes) +
		" even after hunk-level splitting; split the MR manually or raise the per-file budget"
}

// ChunkByFile converts Source values into per-file chunks under
// the given per-chunk byte budget. Files that fit become one
// chunk each; files that overflow are split at hunk boundaries
// (the `@@ ... @@` lines that unified diff uses). A file that
// overflows the budget even after splitting returns
// *OversizedError so the caller can surface it as ExitConfig.
//
// Binary files (Diff == "") are skipped — there's nothing for
// the LLM to review.
//
// maxBytes is the per-chunk budget (a rough proxy for tokens:
// len/4 is the conventional heuristic). Zero or negative
// values return an error.
func ChunkByFile(changes []Source, maxBytes int) ([]Chunk, error) {
	if maxBytes <= 0 {
		return nil, errInvalid("maxBytes must be > 0, got " + itoa(maxBytes))
	}

	var chunks []Chunk
	for _, c := range changes {
		if c.Diff() == "" {
			continue
		}
		wrapped := wrapDiffWithHeader(c)
		size := len(wrapped)

		if size <= maxBytes {
			chunks = append(chunks, Chunk{
				File:       c.Path(),
				OldPath:    c.OldPath(),
				NewPath:    c.NewPath(),
				IsNew:      c.IsNew(),
				IsDeleted:  c.IsDeleted(),
				IsRenamed:  c.IsRenamed(),
				Part:       0,
				TotalParts: 0,
				Diff:       wrapped,
				Size:       size,
			})
			continue
		}

		// Overflow: split at hunk boundaries.
		parts := splitDiffAtHunks(wrapped, maxBytes)
		// If after splitting we still have a part larger than
		// the budget (the smallest hunk doesn't fit), the file
		// can't be reviewed at this budget — surface
		// OversizedError so the operator splits the MR
		// manually.
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
				OldPath:    c.OldPath(),
				NewPath:    c.NewPath(),
				IsNew:      c.IsNew(),
				IsDeleted:  c.IsDeleted(),
				IsRenamed:  c.IsRenamed(),
				Part:       i + 1,
				TotalParts: len(parts),
				Diff:       part,
				Size:       len(part),
			})
		}
	}
	return chunks, nil
}

// wrapDiffWithHeader prefixes a unified-diff body with the
// standard `diff --git` header so a chunk split off from the
// middle of a multi-file diff still parses as a valid hunk.
func wrapDiffWithHeader(c Source) string {
	old := c.OldPath()
	if old == "" {
		old = c.NewPath()
	}
	newPath := c.NewPath()
	if newPath == "" {
		newPath = c.OldPath()
	}

	var b []byte
	b = append(b, "diff --git a/"...)
	b = append(b, old...)
	b = append(b, " b/"...)
	b = append(b, newPath...)
	b = append(b, '\n')
	if c.IsNew() {
		b = append(b, "new file mode 100644\n"...)
	}
	if c.IsDeleted() {
		b = append(b, "deleted file mode 100644\n"...)
	}
	if c.IsRenamed() {
		b = append(b, "rename from "...)
		b = append(b, old...)
		b = append(b, '\n')
		b = append(b, "rename to "...)
		b = append(b, newPath...)
		b = append(b, '\n')
	}
	b = append(b, c.Diff()...)
	return string(b)
}

// splitDiffAtHunks splits a unified-diff body into pieces each
// ≤ maxBytes, preferring to break at `@@ ... @@` lines.
func splitDiffAtHunks(diff string, maxBytes int) []string {
	lines := splitLines(diff)
	hunks := hunkRanges(lines)

	var parts []string
	var current []string
	currentSize := 0

	for _, r := range hunks {
		hunkLines := lines[r.start:r.end]
		hunkSize := totalSize(hunkLines)

		// Single hunk too big: flush current, line-split this
		// hunk, then continue from after it.
		if hunkSize > maxBytes {
			if currentSize > 0 {
				parts = append(parts, joinLines(current))
				current = nil
				currentSize = 0
			}
			parts = append(parts, lineSplit(hunkLines, maxBytes)...)
			continue
		}

		// Would adding this hunk overflow? Flush current.
		if currentSize+hunkSize > maxBytes && len(current) > 0 {
			parts = append(parts, joinLines(current))
			current = nil
			currentSize = 0
		}
		current = append(current, hunkLines...)
		currentSize += hunkSize
	}
	if len(current) > 0 {
		parts = append(parts, joinLines(current))
	}
	return parts
}

type hunkRange struct{ start, end int }

// hunkRanges returns the line ranges that cover each hunk in
// lines.
func hunkRanges(lines []string) []hunkRange {
	var ranges []hunkRange
	preambleEnd := len(lines)
	for i, line := range lines {
		if hasPrefix(line, "@@") && hasSuffix(line, "@@") {
			preambleEnd = i
			break
		}
	}
	if preambleEnd > 0 {
		ranges = append(ranges, hunkRange{start: 0, end: preambleEnd})
	}
	hunkStarts := []int{}
	for i, line := range lines {
		if hasPrefix(line, "@@") && hasSuffix(line, "@@") {
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
// line boundaries.
func lineSplit(lines []string, maxBytes int) []string {
	var parts []string
	current := make([]string, 0, len(lines))
	size := 0
	for _, line := range lines {
		lineSize := len(line) + 1
		if size+lineSize > maxBytes && len(current) > 0 {
			parts = append(parts, joinLines(current))
			current = make([]string, 0, len(lines))
			size = 0
		}
		current = append(current, line)
		size += lineSize
	}
	if len(current) > 0 {
		parts = append(parts, joinLines(current))
	}
	return parts
}

// splitLines splits s on newlines, keeping the trailing empty
// element when s ends with a newline (matches strings.Split
// behaviour).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 64)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start <= len(s) {
		out = append(out, s[start:])
	}
	return out
}

// joinLines joins lines with '\n' separators.
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	total := 0
	for _, l := range lines {
		total += len(l) + 1
	}
	b := make([]byte, 0, total)
	for i, l := range lines {
		if i > 0 {
			b = append(b, '\n')
		}
		b = append(b, l...)
	}
	return string(b)
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
// the joined chunks.
func EstimateTokens(chunks []Chunk) int {
	total := 0
	for _, c := range chunks {
		total += len(c.Diff)
	}
	return total / 4
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

func hasSuffix(s, p string) bool {
	return len(s) >= len(p) && s[len(s)-len(p):] == p
}

// errInvalid returns a sentinel error; kept as a small helper
// because errors.New would pull in the standard library import
// for one use.
func errInvalid(msg string) error { return &invalidErr{msg: msg} }

type invalidErr struct{ msg string }

func (e *invalidErr) Error() string { return "diff: " + e.msg }

// itoa formats an int without importing strconv. Used by the
// OversizedError message so the package stays stdlib-only.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

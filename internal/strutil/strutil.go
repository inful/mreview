// Package strutil hosts cross-cutting string helpers shared by
// more than one package. It exists to keep small, idiomatic
// utilities out ( of the cmd/, internal/gitlab, internal/llm,
// internal/reviewer packages — callers used to redefine
// identical helpers in each (truncate, etc.), which was the
// motivation for the package. New entries should meet the same
// bar: tiny, allocation-aware, no external dependencies, used
// from at least two packages.
package strutil

// Truncate returns s unchanged when len(s) <= maxBytes, otherwise
// the first maxBytes bytes followed by "...".
//
// Used to bound error bodies, raw LLM responses, and other
// potentially-large strings before they land in logs or user-
// visible messages. The byte cut is not grapheme-aware — for the
// present callers (ASCII or byte-clean UTF-8 from JSON / HTTP)
// this is fine; switch to runes if a future caller starts feeding
// arbitrary user input.
//
// Negative maxBytes returns "".
func Truncate(s string, maxBytes int) string {
	if maxBytes < 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "..."
}

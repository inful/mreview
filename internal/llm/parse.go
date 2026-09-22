package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrParseFailure is returned by ParseReviewResponse when no JSON
// could be recovered from the model's reply. The raw content is
// attached so the reviewer can log it for debugging.
type ErrParseFailure struct {
	Raw   string
	Cause error
}

// Error implements error.
func (e *ErrParseFailure) Error() string {
	return fmt.Sprintf("llm: failed to parse review response: %v: %s", e.Cause, truncate(e.Raw, 200))
}

// Unwrap exposes the underlying parser error.
func (e *ErrParseFailure) Unwrap() error { return e.Cause }

// ParseReviewResponse extracts a ReviewResponse from the LLM's raw
// text reply.
//
// Strategies, in order:
//
//  1. raw JSON: try to unmarshal the entire string as
//     ReviewResponse. Catches clean outputs from models that honor
//     response_format=json_object.
//
//  2. fenced JSON: locate a ```json ... ``` block (case-insensitive
//     fence tag) and try to unmarshal it. Catches models that emit
//     a prose preamble / postamble.
//
//  3. loose bracket extraction: find the first balanced { ... } or
//     [ ... ] block that looks like JSON (passes a structural
//     balance check), then try to unmarshal. Catches models that
//     forget the fence or emit a single line of JSON inside a
//     paragraph of prose.
//
// If a bare array of findings is recovered (some models emit only
// the findings without the envelope), we wrap it into a
// ReviewResponse with an empty summary. That's friendlier than
// failing the whole parse.
//
// To prevent accidental matches on unrelated `{ ... }` blocks in
// the prose (e.g. a code sample), an extracted object must carry
// at least one of the ReviewResponse keys ("findings" or
// "summary") or the candidate is skipped.
//
// Strategy 3 is intentionally conservative: we don't try to fix
// malformed JSON. If the model can't follow instructions, we want
// to surface that loudly — not silently produce an empty
// Findings slice.
func ParseReviewResponse(raw string) (ReviewResponse, error) {
	if strings.TrimSpace(raw) == "" {
		return ReviewResponse{}, &ErrParseFailure{Raw: raw, Cause: errors.New("empty response")}
	}

	// 1. raw JSON — try envelope first, then bare array.
	if resp, ok := tryUnmarshalEnvelope(raw); ok {
		return resp, nil
	}
	if resp, ok := tryUnmarshalBareArray(raw); ok {
		return resp, nil
	}

	// 2. fenced ```json ... ```
	if extracted := extractFenced(raw, "json"); extracted != "" {
		if resp, ok := tryUnmarshalEnvelope(extracted); ok {
			return resp, nil
		}
		if resp, ok := tryUnmarshalBareArray(extracted); ok {
			return resp, nil
		}
	}
	if extracted := extractFenced(raw, ""); extracted != "" {
		if resp, ok := tryUnmarshalEnvelope(extracted); ok {
			return resp, nil
		}
		if resp, ok := tryUnmarshalBareArray(extracted); ok {
			return resp, nil
		}
	}

	// 3. loose bracket extraction
	if extracted := extractLoose(raw); extracted != "" {
		// Loose extractor may grab a non-envelope object; validate.
		if hasReviewKeys(extracted) {
			if resp, ok := tryUnmarshalEnvelope(extracted); ok {
				return resp, nil
			}
		}
		// A loose [...] always means bare findings (the model emitted
		// just the array).
		if strings.HasPrefix(strings.TrimSpace(extracted), "[") {
			if resp, ok := tryUnmarshalBareArray(extracted); ok {
				return resp, nil
			}
		}
	}

	return ReviewResponse{}, &ErrParseFailure{
		Raw:   raw,
		Cause: errors.New("no JSON recovered via raw / fenced / loose extraction"),
	}
}

// tryUnmarshalEnvelope tries to unmarshal s as ReviewResponse and
// returns the result only when the resulting object has at least
// one of the schema keys present in the source. A bare object that
// just happens to parse but carries none of our keys is rejected
// so the loose-extractor path doesn't grab a stray code-sample
// object.
func tryUnmarshalEnvelope(s string) (ReviewResponse, bool) {
	if !hasReviewKeys(s) {
		return ReviewResponse{}, false
	}
	var resp ReviewResponse
	if err := json.Unmarshal([]byte(s), &resp); err != nil {
		return ReviewResponse{}, false
	}
	return resp, true
}

// tryUnmarshalBareArray tries to unmarshal s as a bare findings
// array. Only valid when s is a JSON array (not an object).
func tryUnmarshalBareArray(s string) (ReviewResponse, bool) {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "[") {
		return ReviewResponse{}, false
	}
	var findings []Finding
	if err := json.Unmarshal([]byte(s), &findings); err != nil {
		return ReviewResponse{}, false
	}
	return ReviewResponse{Findings: findings}, true
}

// hasReviewKeys reports whether s, as a JSON object, contains at
// least one of the ReviewResponse keys. We use a streaming
// decoder so we don't pay full unmarshal cost just to check.
func hasReviewKeys(s string) bool {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	// Read the opening brace.
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return false
	}
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return false
		}
		key, ok := tok.(string)
		if !ok {
			return false
		}
		if key == "findings" || key == "summary" {
			return true
		}
		// Skip the value.
		if _, err := dec.Token(); err != nil {
			return false
		}
	}
	return false
}

// extractFenced returns the content of the first fenced code block
// whose opening fence is tagged with `lang` (case-insensitive). If
// lang is empty, the first fenced block of any kind is returned.
func extractFenced(raw, lang string) string {
	const openFence = "```"
	idx := strings.Index(raw, openFence)
	if idx < 0 {
		return ""
	}
	// Walk past the opening fence + tag.
	rest := raw[idx+len(openFence):]
	// The tag (if any) ends at the next newline.
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		return ""
	}
	tag := strings.TrimSpace(rest[:nl])
	rest = rest[nl+1:]

	if lang != "" && !strings.EqualFold(tag, lang) {
		// Try the next fence; recursion-free via a loop.
		tail := raw[idx+len(openFence)+len(tag)+1:]
		if next := extractFenced(tail, lang); next != "" {
			return next
		}
		return ""
	}

	// Find the matching closing fence.
	closeIdx := strings.Index(rest, openFence)
	if closeIdx < 0 {
		// Unclosed fence: take everything to the end.
		return strings.TrimRight(rest, "\n")
	}
	return rest[:closeIdx]
}

// extractLoose finds the first balanced JSON block ({...} or [...])
// in raw. Returns "" when none can be located.
//
// We only accept a block that:
//   - starts with `{` or `[`
//   - has matching opening/closing braces
//   - has balanced string quoting
//
// This isn't a full JSON parser; it's a structural sniff to avoid
// matching `for (i=0; i<n; i++)` in a code block. Anything
// structurally invalid gets skipped.
func extractLoose(raw string) string {
	b := []byte(raw)
	for i := 0; i < len(b); i++ {
		if b[i] != '{' && b[i] != '[' {
			continue
		}
		// Try to balance from this index.
		end, ok := findMatching(b[i:], b[i])
		if !ok {
			continue
		}
		return string(b[i : i+end+1])
	}
	return ""
}

// findMatching returns the index (relative to s) of the closing
// brace that matches s[0], or (0, false) if the block is
// unbalanced / has unterminated strings.
func findMatching(s []byte, open byte) (int, bool) {
	closer := byte('}')
	if open == '[' {
		closer = ']'
	}
	depth := 0
	inString := false
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch c {
			case '\\':
				escape = !escape
			case '"':
				if !escape {
					inString = false
				}
				escape = false
			default:
				escape = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// truncate returns the first n characters of s with "..." appended
// when it would be cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

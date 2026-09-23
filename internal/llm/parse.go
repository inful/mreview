package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/inful/mreview/internal/strutil"
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
	return fmt.Sprintf("llm: failed to parse review response: %v: %s", e.Cause, strutil.Truncate(e.Raw, 200))
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
//  4. streaming partial extraction: when the response was
//     truncated mid-stream (LLM hit MaxTokens, network cutoff,
//     server overload — observed in production), the previous
//     three strategies all fail because findMatching never sees
//     the closing brace / quote and raw Unmarshal hits
//     ErrUnexpectedEOF. Strategy 4 uses a streaming json.Decoder
//     to walk the response and recover whatever COMPLETE
//     findings the LLM emitted before the truncation point.
//     Findings are returned even if the response ends mid-finding
//     or mid-summary — partial findings are silently dropped (the
//     decoder fails on them, so we move on), and we keep whatever
//     summary prefix we managed to read.
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

	// 4. streaming partial extraction — recovery for truncated
	// responses. Returns whatever complete findings the LLM
	// emitted before the cut. Only used when strategies 1-3 fail;
	// if the response was structurally complete, we wouldn't be
	// here.
	if findings, summary, ok := extractStreamingFindings(raw); ok && len(findings) > 0 {
		return ReviewResponse{Findings: findings, Summary: summary}, nil
	}

	return ReviewResponse{}, &ErrParseFailure{
		Raw:   raw,
		Cause: errors.New(errNoRecovery(raw)),
	}
}

// errNoRecovery returns a more diagnostic error message that
// distinguishes "truncated mid-string" from "invalid JSON" so
// operators reading parse-failure logs can immediately tell which
// fix to apply (raise MaxTokens / batch size vs. file an LLM bug).
//
// Returns a static message when the truncation detection is
// inconclusive.
func errNoRecovery(raw string) string {
	trimmed := strings.TrimRight(raw, " \t\r\n")
	if trimmed == "" {
		return "no JSON recovered via raw / fenced / loose extraction (empty after trim)"
	}
	if looksTruncated(trimmed) {
		return fmt.Sprintf(
			"no JSON recovered via raw / fenced / loose extraction (response appears truncated: len=%d, ends mid-string or mid-object — likely MaxTokens or network cutoff)",
			len(raw),
		)
	}
	return "no JSON recovered via raw / fenced / loose extraction (response is structurally unbalanced but does not look truncated — likely invalid JSON from LLM)"
}

// looksTruncated reports whether raw ends in a state that
// strongly suggests the response was cut off mid-generation
// rather than the LLM producing invalid JSON. Two signals count:
//
//  1. ends with an unterminated string (last char is not the
//     closing quote of a complete JSON value), OR
//  2. ends inside a structural delimiter — `,` or `:` with no
//     following value, suggesting the LLM was about to emit
//     more content.
func looksTruncated(raw string) bool {
	if raw == "" {
		return false
	}
	// Strip trailing whitespace for the heuristic.
	trimmed := strings.TrimRight(raw, " \t\r\n")
	if trimmed == "" {
		return false
	}
	last := trimmed[len(trimmed)-1]
	// A response that ends with `}` or `]` is structurally
	// complete at the outermost level (the LLM finished its
	// output). If it's still unparseable, the JSON is invalid
	// rather than truncated.
	switch last {
	case '}', ']':
		return false
	}
	// Anything else: ends with `,`, `:`, `"`, alphanumeric, etc.
	// — all signs of mid-stream cutoff. The balanced-brace
	// strategies would have caught a complete outer object, so
	// we're here because one didn't materialise.
	return true
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
		// Try the next fence via recursion. The depth is bounded
		// by the number of fences in `raw`; pathological inputs
		// could in principle blow the stack, but real LLM output
		// has at most a handful of fenced blocks.
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

// extractStreamingFindings uses a json.Decoder to walk the raw
// response and extract whatever complete findings the LLM
// emitted before any truncation. It returns the recovered
// findings, the recovered summary (may be empty), and a flag
// indicating whether the response was recognisably a
// ReviewResponse envelope (had an opening `{` and a "findings"
// or "summary" key).
//
// The streaming approach is robust to truncation because the
// decoder consumes complete JSON values one at a time and
// returns an error only when it hits invalid or missing data.
// Findings that completed BEFORE the truncation point are
// returned; the partially-written finding at the truncation
// point fails to decode and is silently dropped.
//
// The decoder stops at the first decode error and returns. Any
// unread trailing content (summary fragments, closing braces)
// is dropped. This is deliberate: returning partial JSON that
// the caller would then have to validate adds risk for
// negligible value.
//
// Why this is its own strategy (and not a fallback on
// strategies 1-3): if the response was structurally complete,
// one of 1-3 would have unmarshalled it. We're here because
// the response ends mid-string or mid-object — findMatching
// never balances, raw Unmarshal hits ErrUnexpectedEOF, no
// fence. The streaming decoder doesn't care about the outer
// brace balance; it just walks the tokens until it hits
// invalid data.
func extractStreamingFindings(raw string) (findings []Finding, summary string, found bool) {
	dec := json.NewDecoder(strings.NewReader(raw))

	// Read opening brace.
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, "", false
	}

	// Walk top-level keys.
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := keyTok.(string)
		if !ok {
			break
		}

		switch key {
		case "findings":
			found = true
			// Consume the array opening '['.
			tok, err := dec.Token()
			if err != nil || tok != json.Delim('[') {
				return findings, summary, found
			}
			// Stream findings until error or end of array.
			for dec.More() {
				var f Finding
				if err := dec.Decode(&f); err != nil {
					// Truncated mid-finding or invalid — stop.
					// Whatever we already have is the result.
					return findings, summary, found
				}
				findings = append(findings, f)
			}
			// Consume closing ']'.
			_, _ = dec.Token()
		case "summary":
			tok, err := dec.Token()
			if err != nil {
				return findings, summary, found
			}
			if s, ok := tok.(string); ok {
				summary = s
			}
		default:
			// Skip unknown top-level key's value so the
			// decoder doesn't choke on it.
			if _, err := dec.Token(); err != nil {
				return findings, summary, found
			}
		}
	}
	return findings, summary, found
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

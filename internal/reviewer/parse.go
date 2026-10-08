package reviewer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ParseReviewResponse extracts a ReviewResponse from the
// agent's raw text reply. The system prompt instructs the
// model to emit a single JSON object with no prose and no
// Markdown fences, and the canonical parse handles that.
// In practice, reasoning models (Claude with extended
// thinking, smaller local models with chain-of-thought)
// often emit inline reasoning BEFORE the final JSON object;
// a strict "no prose" instruction fights the model's
// training. This parser tries three paths in order:
//
//  1. The whole body parses as JSON (the canonical case).
//  2. The body wrapped in a single ```json ... ``` fence
//     (the alternative the system prompt allows).
//  3. The first balanced { ... } object in the body
//     (the fallback for prose-wrapped output).
//
// Returns a ReviewResponse or a parse error. The error
// string does NOT include a "parse: " prefix — the
// orchestrator's call site adds the "orchestrator: parse: "
// prefix to the error chain. (The previous implementation
// added a duplicate "parse: " prefix that ended up in the
// operator-facing log as "orchestrator: parse: parse: <err>".)
func ParseReviewResponse(raw string) (ReviewResponse, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ReviewResponse{}, errors.New("empty response")
	}

	var resp ReviewResponse
	var lastErr error

	// Path 1: the whole body is the JSON.
	lastErr = json.Unmarshal([]byte(trimmed), &resp)
	if lastErr == nil {
		return resp, nil
	}

	// Path 2: JSON wrapped in a ```json ... ``` fence.
	body := trimmed
	if strings.HasPrefix(body, "```") {
		body = stripFences(body)
		if err := json.Unmarshal([]byte(body), &resp); err == nil {
			return resp, nil
		} else {
			lastErr = err
		}
	}

	// Path 3: first balanced JSON object embedded in
	// reasoning prose. This is the production failure
	// mode captured in the 2026-10-07 log: a smaller
	// local model (qwen2.5-coder:7b) emitted ~3000
	// bytes of inline reasoning before a valid JSON
	// object. The fallback recovers the JSON; the
	// orchestrator continues as if the model had
	// behaved. The "no prose" instruction is still
	// in the system prompt; this is a safety net.
	if jsonStr, ok := extractFirstJSON(trimmed); ok {
		if err := json.Unmarshal([]byte(jsonStr), &resp); err == nil {
			return resp, nil
		} else {
			lastErr = err
		}
	}

	return ReviewResponse{}, fmt.Errorf("could not find valid JSON in response: %w", lastErr)
}

// stripFences removes the outer ``` ... ``` (or ```json ...```)
// wrapper if the response starts and ends with one. Doesn't
// try to be clever — it just trims the first and last lines
// when they look like fences. An unclosed fence (open with no
// close) is left intact so the JSON parser can fail
// informatively.
func stripFences(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "```") {
		return s
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "```") {
		return s
	}
	return strings.Join(lines[1:len(lines)-1], "\n")
}

// extractFirstJSON finds the first balanced JSON object in s
// and returns the substring. Returns "" and false if no
// balanced object is found.
//
// Walks the string tracking brace depth, respecting JSON
// string literals (so braces inside strings don't throw off
// the count) and JSON escape sequences (so \" inside a
// string doesn't prematurely end the string). When depth
// returns to 0 after being > 0, the substring from the
// matching '{' to this '}' is the first complete object.
//
// UTF-8 handling: the rune is decoded via utf8.DecodeRuneInString
// so a multi-byte character whose first byte happens to be
// '{' or '}' (extremely unlikely in well-formed UTF-8, but
// possible in some encodings) doesn't get miscounted as a
// JSON structural character.
func extractFirstJSON(s string) (string, bool) {
	var start, depth = -1, 0
	var inString, escaped bool

	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])

		if inString {
			switch {
			case escaped:
				// Previous char was a backslash inside a
				// string; this char is escaped regardless
				// of what it is. Clear the flag and
				// continue.
				escaped = false
			case r == '\\':
				escaped = true
			case r == '"':
				// End of string. Stay in non-string mode
				// from here on (braces and other JSON
				// structural chars count again).
				inString = false
			}
			i += size
			continue
		}

		switch r {
		case '"':
			// Enter string mode. The opening quote is
			// not part of the string content; it's the
			// delimiter.
			inString = true
		case '{':
			if depth == 0 {
				// Record the start of the outermost
				// object. We overwrite this if a
				// previous '{' was abandoned (e.g.
				// inside an unterminated string), but
				// the brace counter only goes > 0 on
				// real opens.
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					// First complete object.
					// Return the slice from the
					// matching '{' through this '}'.
					return s[start : i+size], true
				}
			}
		}
		i += size
	}
	return "", false
}

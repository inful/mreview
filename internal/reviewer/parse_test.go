package reviewer

import (
	"testing"
)

// TestParseReviewResponse_ProseBeforeJSON is the regression
// test for the 2026-10-07 production failure: a smaller
// local model (qwen2.5-coder:7b via Ollama) emitted ~3000
// bytes of inline reasoning prose BEFORE the final JSON
// object. The previous parser failed with "invalid character
// 'I' looking for beginning of value" at the start of "I'll
// start by reading...". The path 3 fallback (extract the
// first balanced JSON object) recovers the response and the
// review continues.
//
// The original happy-path / fenced / empty / invalid tests
// (TestParseReviewResponse_RawJSON, _Fenced, _Empty,
// _InvalidJSON, TestStripFences) live in orchestrator_test.go
// from before this file existed.
func TestParseReviewResponse_ProseBeforeJSON(t *testing.T) {
	raw := `I'll start by reading the diff against the contract. Here's my analysis.

**Prior findings:**
- #1: resolved because of the new commit.
- #2: still valid.

{
  "findings": [
    {"file": "a.go", "line": 10, "severity": "warning", "category": "test", "body": "Missing test", "suggestion": ""}
  ],
  "prior_findings": [
    {"file": "b.go", "line": 20, "status": "resolved", "rationale": "fixed by commit"},
    {"file": "c.go", "line": 30, "status": "still_valid"}
  ],
  "summary": "Mostly clean."
}`
	resp, err := ParseReviewResponse(raw)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if len(resp.Findings) != 1 {
		t.Errorf("findings = %d, want 1", len(resp.Findings))
	}
	if len(resp.PriorFindings) != 2 {
		t.Errorf("prior_findings = %d, want 2", len(resp.PriorFindings))
	}
	if resp.Summary != "Mostly clean." {
		t.Errorf("summary = %q", resp.Summary)
	}
}

// TestParseReviewResponse_ProseOnBothSides covers the
// case where the LLM emits prose both before and after
// the JSON object. The path 3 fallback recovers the JSON;
// trailing prose after the closing '}' is naturally
// excluded because the extractor stops at the first
// balanced object.
func TestParseReviewResponse_ProseOnBothSides(t *testing.T) {
	raw := `Let me think about this...
{"summary": "ok"}
That should do it.`
	resp, err := ParseReviewResponse(raw)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if resp.Summary != "ok" {
		t.Errorf("summary = %q, want ok", resp.Summary)
	}
}

// TestParseReviewResponse_ProductionLog is the
// regression test for the actual 2026-10-07 production
// failure. The response is structured exactly like the
// raw LLM output captured in the log: ~3000 bytes of
// inline reasoning with bold-prefixed markdown analysis,
// followed by a valid JSON object. The fallback should
// recover the JSON.
func TestParseReviewResponse_ProductionLog(t *testing.T) {
	raw := `I'll start by reading the skill-authoring guidance (required since this MR authors a new skill file) and listing available skills.
I've reviewed the diff against the skill-authoring contract and the prior findings. Here's my analysis.
**Prior findings:**
- **#1 (ca-bundle-sync artifact path):** Now resolved — ` + "`artifacts:paths`" + ` is the literal ` + "`.mreview-artifacts/ca-bundle.pem`" + `.
- **#2 (debug instrumentation):** Still valid.
- **#3 (trailing newline / MD047):** Resolved.
**New findings:** the ` + "`--gitlab-url`" + ` variable change, the Docker Hub image switch, and the over-long skill body.
My analysis: all three priors addressed.
{
  "findings": [
    {"file": ".gitlab-ci.yml", "line": 148, "severity": "warning", "category": "correctness", "body": "double-prefix risk", "suggestion": ""}
  ],
  "prior_findings": [
    {"file": ".gitlab-ci.yml", "line": 157, "status": "resolved", "rationale": "artifacts:paths now matches"},
    {"file": ".gitlab-ci.yml", "line": 177, "status": "still_valid", "rationale": "debug flags still present"}
  ],
  "summary": "Prior CI regression fixed."
}`
	resp, err := ParseReviewResponse(raw)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if len(resp.Findings) != 1 {
		t.Errorf("findings = %d, want 1", len(resp.Findings))
	}
	if len(resp.PriorFindings) != 2 {
		t.Errorf("prior_findings = %d, want 2", len(resp.PriorFindings))
	}
	if resp.Summary != "Prior CI regression fixed." {
		t.Errorf("summary = %q", resp.Summary)
	}
}

// TestParseReviewResponse_MalformedJSONInExtraction covers
// the edge case where path 1 and path 2 fail, and the
// extracted candidate is also not valid JSON. We should
// fall through to the error, not silently return garbage.
func TestParseReviewResponse_MalformedJSONInExtraction(t *testing.T) {
	raw := `Some prose. Then a malformed object: {"a": 1, "b":} (missing value).`
	_, err := ParseReviewResponse(raw)
	if err == nil {
		t.Error("malformed JSON in extracted candidate should return an error")
	}
}

// TestExtractFirstJSON_Simple covers the basic case.
func TestExtractFirstJSON_Simple(t *testing.T) {
	s := `prefix {"a": 1, "b": 2} suffix`
	got, ok := extractFirstJSON(s)
	if !ok {
		t.Fatal("extractFirstJSON returned false")
	}
	want := `{"a": 1, "b": 2}`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestExtractFirstJSON_Nested covers nested objects. The
// outer '{' must match the outer '}', not the inner '}'.
func TestExtractFirstJSON_Nested(t *testing.T) {
	s := `prefix {"a": {"b": 1}, "c": [1, 2, 3]} suffix`
	got, ok := extractFirstJSON(s)
	if !ok {
		t.Fatal("extractFirstJSON returned false")
	}
	want := `{"a": {"b": 1}, "c": [1, 2, 3]}`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestExtractFirstJSON_StringsWithBraces covers braces
// inside JSON string literals (which must not throw off
// the depth counter).
func TestExtractFirstJSON_StringsWithBraces(t *testing.T) {
	s := `prefix {"body": "has { and } inside"} suffix`
	got, ok := extractFirstJSON(s)
	if !ok {
		t.Fatal("extractFirstJSON returned false")
	}
	want := `{"body": "has { and } inside"}`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestExtractFirstJSON_EscapedQuotes covers escaped quotes
// inside JSON string literals (which must not prematurely
// end the string).
func TestExtractFirstJSON_EscapedQuotes(t *testing.T) {
	s := `prefix {"body": "he said \"hi\""} suffix`
	got, ok := extractFirstJSON(s)
	if !ok {
		t.Fatal("extractFirstJSON returned false")
	}
	want := `{"body": "he said \"hi\""}`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestExtractFirstJSON_NoJSON covers the case where there's
// no JSON object at all.
func TestExtractFirstJSON_NoJSON(t *testing.T) {
	s := `just text without any json`
	_, ok := extractFirstJSON(s)
	if ok {
		t.Error("extractFirstJSON returned true for non-JSON text")
	}
}

// TestExtractFirstJSON_Unbalanced covers the case where
// there's a { but no matching }.
func TestExtractFirstJSON_Unbalanced(t *testing.T) {
	s := `prefix {this is not json`
	_, ok := extractFirstJSON(s)
	if ok {
		t.Error("extractFirstJSON returned true for unbalanced text")
	}
}

// TestExtractFirstJSON_MultipleObjects covers the case
// where the LLM emits two JSON objects (e.g. an intermediate
// and a final). The first one wins.
func TestExtractFirstJSON_MultipleObjects(t *testing.T) {
	s := `first {"a": 1} second {"b": 2}`
	got, ok := extractFirstJSON(s)
	if !ok {
		t.Fatal("extractFirstJSON returned false")
	}
	want := `{"a": 1}`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestExtractFirstJSON_Empty covers the empty case.
func TestExtractFirstJSON_Empty(t *testing.T) {
	_, ok := extractFirstJSON("")
	if ok {
		t.Error("extractFirstJSON returned true for empty string")
	}
}

// TestExtractFirstJSON_OnlyOpenBrace covers a single
// opening brace with no content.
func TestExtractFirstJSON_OnlyOpenBrace(t *testing.T) {
	_, ok := extractFirstJSON("{")
	if ok {
		t.Error("extractFirstJSON returned true for '{' alone")
	}
}

// TestExtractFirstJSON_MultibyteRune covers the UTF-8
// case: a multi-byte character in the prose should not
// affect the brace counter. The grinning-face emoji
// (U+1F600) is 4 bytes in UTF-8; the loop must advance
// by the byte size, not by 1.
func TestExtractFirstJSON_MultibyteRune(t *testing.T) {
	s := `prefix 😄 {"a": 1} suffix`
	got, ok := extractFirstJSON(s)
	if !ok {
		t.Fatal("extractFirstJSON returned false")
	}
	want := `{"a": 1}`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestExtractFirstJSON_StringWithEscapedBackslash covers
// \\ inside a string: the first \ sets escaped=true, the
// second \ is consumed as the escaped char (clearing
// escaped), the following " does NOT end the string.
// Common in regex / path patterns.
func TestExtractFirstJSON_StringWithEscapedBackslash(t *testing.T) {
	s := `prefix {"path": "C:\\Users\\\"weird\\\"name"} suffix`
	got, ok := extractFirstJSON(s)
	if !ok {
		t.Fatal("extractFirstJSON returned false")
	}
	want := `{"path": "C:\\Users\\\"weird\\\"name"}`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

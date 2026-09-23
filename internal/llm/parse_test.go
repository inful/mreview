package llm

import (
	"errors"
	"strings"
	"testing"
)

func TestParseReviewResponse_RawJSON(t *testing.T) {
	in := `{"findings":[{"file":"a.go","line":1,"severity":"warning","category":"security","body":"x"}],"summary":"LGTM"}`
	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if got.Summary != "LGTM" {
		t.Errorf("Summary = %q", got.Summary)
	}
	if len(got.Findings) != 1 || got.Findings[0].File != "a.go" {
		t.Errorf("Findings = %+v", got.Findings)
	}
	if got.Findings[0].Severity != SeverityWarning {
		t.Errorf("Severity = %q", got.Findings[0].Severity)
	}
}

func TestParseReviewResponse_FencedJSON(t *testing.T) {
	in := "Here's my review:\n\n```json\n" +
		`{"findings":[{"file":"a.go","line":2,"severity":"error","body":"oops"}],"summary":"fix"}` +
		"\n```\n\nHope that helps!"
	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if len(got.Findings) != 1 || got.Findings[0].File != "a.go" {
		t.Errorf("Findings = %+v", got.Findings)
	}
	if got.Findings[0].Severity != SeverityError {
		t.Errorf("Severity = %q", got.Findings[0].Severity)
	}
}

func TestParseReviewResponse_FencedJSON_UppercaseTag(t *testing.T) {
	// Some models emit ```JSON ... ``` with an uppercase tag.
	in := "```JSON\n{\"findings\":[],\"summary\":\"all good\"}\n```"
	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if got.Summary != "all good" {
		t.Errorf("Summary = %q", got.Summary)
	}
}

func TestParseReviewResponse_FencedPlain(t *testing.T) {
	// Model forgot the language tag.
	in := "```\n{\"findings\":[],\"summary\":\"plain fence\"}\n```"
	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if got.Summary != "plain fence" {
		t.Errorf("Summary = %q", got.Summary)
	}
}

func TestParseReviewResponse_LooseJSON(t *testing.T) {
	in := "Sure, here's what I found: {\"findings\":[{\"file\":\"x\",\"line\":7,\"body\":\"y\"}],\"summary\":\"ok\"} and that's it."
	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if got.Summary != "ok" {
		t.Errorf("Summary = %q", got.Summary)
	}
	if len(got.Findings) != 1 || got.Findings[0].Line != 7 {
		t.Errorf("Findings = %+v", got.Findings)
	}
}

func TestParseReviewResponse_LooseJSON_Array(t *testing.T) {
	// Some models emit just the findings array without an envelope.
	in := "Findings: [{\"file\":\"a.go\",\"line\":1,\"body\":\"x\"},{\"file\":\"b.go\",\"line\":2,\"body\":\"y\"}]"
	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if len(got.Findings) != 2 {
		t.Errorf("Findings len = %d, want 2", len(got.Findings))
	}
}

func TestParseReviewResponse_Empty(t *testing.T) {
	_, err := ParseReviewResponse("")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	var pfe *ErrParseFailure
	if !errors.As(err, &pfe) {
		t.Errorf("expected *ErrParseFailure, got %T", err)
	}
}

func TestParseReviewResponse_Unparseable(t *testing.T) {
	_, err := ParseReviewResponse("The model wrote only prose with no JSON anywhere to be found.")
	if err == nil {
		t.Fatal("expected error")
	}
	var pfe *ErrParseFailure
	if !errors.As(err, &pfe) {
		t.Errorf("expected *ErrParseFailure, got %T", err)
	}
	if !strings.Contains(pfe.Error(), "no JSON recovered") {
		t.Errorf("error message should mention strategy, got %q", pfe.Error())
	}
}

func TestParseReviewResponse_TruncatedJSON_FallsThroughToLoose(t *testing.T) {
	// Truncated envelope where the inner findings array is still
	// complete: the loose extractor matches the array and we
	// recover a one-element ReviewResponse (no summary).
	in := `{"findings":[{"file":"a.go","line":1,"body":"x"}],`
	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("expected recovery via bare-array extraction, got %v", err)
	}
	if len(got.Findings) != 1 || got.Findings[0].File != "a.go" {
		t.Errorf("Findings = %+v", got.Findings)
	}
	if got.Summary != "" {
		t.Errorf("Summary should be empty, got %q", got.Summary)
	}
}

func TestParseReviewResponse_GenuinelyUnparseable(t *testing.T) {
	// A truncated JSON where no balanced block can be recovered —
	// both the inner and outer blocks are incomplete.
	in := `prefix text { "findings": [ { "file": "a.go", "line": 1, "body": "x"`
	_, err := ParseReviewResponse(in)
	if err == nil {
		t.Fatal("expected error for unparseable input")
	}
}

func TestParseReviewResponse_FencedJSON_RecoversFromUnclosedFence(t *testing.T) {
	// Model emitted a fence with no closing tag. We should treat
	// the rest of the string as the content (best effort).
	in := "```json\n{\"summary\":\"unclosed fence\",\"findings\":[]}"
	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if got.Summary != "unclosed fence" {
		t.Errorf("Summary = %q", got.Summary)
	}
}

func TestExtractLoose_HandlesBracesInsideStrings(t *testing.T) {
	// A string with a `{` and `}` should not unbalance our matching.
	got := extractLoose(`prefix { "outer": "{ not nested }", "inner": 1 } suffix`)
	if !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") {
		t.Errorf("extractLoose got %q", got)
	}
}

func TestExtractLoose_ReturnsEmptyOnNoJSON(t *testing.T) {
	if got := extractLoose("just plain text, no braces"); got != "" {
		t.Errorf("extractLoose = %q, want empty", got)
	}
}

func TestExtractLoose_ReturnsEmptyOnUnbalanced(t *testing.T) {
	if got := extractLoose(`{ "unbalanced": true`); got != "" {
		t.Errorf("extractLoose = %q, want empty on unbalanced", got)
	}
}

func TestExtractFenced_NoFence(t *testing.T) {
	if got := extractFenced("plain text", "json"); got != "" {
		t.Errorf("extractFenced = %q, want empty", got)
	}
}

// TestParseReviewResponse_Streaming_TruncatedMidString pins the
// recovery path for the production failure mode observed in
// 2026-09: the LLM hit MaxTokens mid-string while generating the
// 3rd finding. Strategies 1-3 all fail because:
//   - raw Unmarshal hits ErrUnexpectedEOF
//   - no fence is present
//   - findMatching never sees the closing quote so the loose
//     extractor returns ""
//
// Strategy 4 (streaming partial extraction) must recover the
// first 2 complete findings and drop the truncated 3rd.
func TestParseReviewResponse_Streaming_TruncatedMidString(t *testing.T) {
	in := `{"findings":[` +
		`{"file":"a.go","line":1,"severity":"warning","category":"security","body":"first"},{"file":"b.go","line":2,"severity":"error","category":"correctness","body":"second"},{"file":"c.go","line":3,"severity":"warning","category":"correctness","body":"third `

	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("recovered %d findings, want 2 (truncated 3rd must be dropped)", len(got.Findings))
	}
	if got.Findings[0].File != "a.go" || got.Findings[1].File != "b.go" {
		t.Errorf("recovered findings = %+v, want a.go and b.go", got.Findings)
	}
}

// TestParseReviewResponse_Streaming_TruncatedMidObject: truncation
// in the middle of a finding object (closing braces never
// written). Streaming extraction must stop at the last complete
// finding and return it.
func TestParseReviewResponse_Streaming_TruncatedMidObject(t *testing.T) {
	in := `{"findings":[{"file":"a.go","line":1,"severity":"warning","category":"security","body":"first"},{"file":"b.go","line":2,"severity":"error","category":"correctness","body":"sec`

	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("recovered %d findings, want 1 (truncated mid-object must be dropped)", len(got.Findings))
	}
	if got.Findings[0].File != "a.go" {
		t.Errorf("recovered finding = %+v, want a.go", got.Findings[0])
	}
}

// TestParseReviewResponse_Streaming_NoFindingsRecovered: truncation
// happens before any finding completes (e.g. response ends after
// the opening `"findings": [`). Strategies 1-3 fail; strategy 4
// must NOT return a partial/empty ReviewResponse — it must surface
// the parse failure loudly. The downstream filterFindings will not
// silently drop a real MR.
func TestParseReviewResponse_Streaming_NoFindingsRecovered(t *testing.T) {
	in := `{"findings":[{"file":"a.go","line":1,"severity":"warning"`

	_, err := ParseReviewResponse(in)
	if err == nil {
		t.Fatal("ParseReviewResponse succeeded with truncated mid-finding; want error")
	}
	// Error must include the truncation diagnostic so operators
	// can tell this was a MaxTokens / cutoff issue (not invalid
	// JSON from the LLM).
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error missing truncation diagnostic: %v", err)
	}
}

// TestParseReviewResponse_Streaming_CompleteResponse_StillUsesFirstStrategy:
// when the response is structurally complete, strategies 1-3 must
// handle it (faster than streaming). Streaming is a fallback only.
func TestParseReviewResponse_Streaming_CompleteResponse_StillUsesFirstStrategy(t *testing.T) {
	in := `{"findings":[{"file":"a.go","line":1,"severity":"warning","category":"security","body":"x"}],"summary":"all good"}`
	got, err := ParseReviewResponse(in)
	if err != nil {
		t.Fatalf("ParseReviewResponse: %v", err)
	}
	if got.Summary != "all good" || len(got.Findings) != 1 {
		t.Errorf("complete response parsed wrong: %+v", got)
	}
}

// TestParseReviewResponse_InvalidJSON_NotMistakenForTruncation: when
// the response ends with `}` (structurally complete outer
// envelope) but the content is invalid JSON (LLM bug, not
// truncation), the diagnostic must NOT call it "truncated" —
// operators need to know the difference between "raise MaxTokens"
// and "file an LLM bug".
//
// The test uses a syntactically invalid inner value (an object
// where a string is expected, missing colon and quotes). Strategy
// 1 (raw Unmarshal) and strategy 3 (loose) both fail; strategy 4
// (streaming) also fails on the invalid value. The response ends
// with `}` so looksTruncated must return false.
func TestParseReviewResponse_InvalidJSON_NotMistakenForTruncation(t *testing.T) {
	in := `{"summary": {"not": "a string"}}`

	_, err := ParseReviewResponse(in)
	if err == nil {
		t.Fatal("ParseReviewResponse succeeded with bad JSON; want error")
	}
	if strings.Contains(err.Error(), "response appears truncated") {
		t.Errorf("error wrongly labelled truncated; bad JSON should NOT match looksTruncated: %v", err)
	}
}

// TestLooksTruncated: pin the heuristic that distinguishes
// truncation (LLM hit MaxTokens mid-stream) from invalid JSON
// (LLM produced bad output). Used by the parse-error diagnostic.
func TestLooksTruncated(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"only whitespace", "  \n\t  ", false},
		{"ends with brace (complete)", `{"a":1}`, false},
		{"ends with bracket (complete)", `[1,2,3]`, false},
		{"ends mid-string (truncated)", `{"body":"unfinished`, true},
		{"ends with comma (truncated)", `{"a":1,`, true},
		{"ends with colon (truncated)", `{"a":`, true},
		{"ends with alphanumeric (truncated)", `{"a":"abc`, true},
		{"ends with newline after completion", "{\"a\":1}\n", false},
		{"ends with whitespace after completion", "{\"a\":1}   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksTruncated(tc.in); got != tc.want {
				t.Errorf("looksTruncated(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

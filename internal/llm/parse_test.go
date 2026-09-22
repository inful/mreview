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

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"a longish string", 5, "a lon..."},
		{"", 5, ""},
	}
	for _, tc := range cases {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

package decision

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inful/decide"
)

// silentLogger discards all output. The decision API
// adapter logs at debug level; the tests assert on
// observable behaviour, not log output, so a quiet
// logger keeps the test runner output clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newFakeServer stands up an httptest server that
// responds to /v1/systemone with a canned decision
// response. Tests can override the response per-case
// by setting fields on the returned closure.
//
// capture is recorded per request: the request body,
// the state body, and the questions map. Tests assert
// on these to verify the request shape.
func newFakeServer(t *testing.T, resp decide.Response) (string, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var req decide.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		c.requestBodies = append(c.requestBodies, req)
		c.stateBodies = append(c.stateBodies, req.State.Body)
		c.questions = append(c.questions, req.Questions)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, c
}

type capture struct {
	requestBodies []decide.Request
	stateBodies   []string
	questions     []decide.Questions
}

// TestAdapter_ClassifyPrior_Resolved is the happy path:
// the decision API returns "resolved" with high
// confidence, the adapter maps it to StatusResolved and
// surfaces the per-class probabilities. The test also
// asserts on the request shape (state body carries the
// prior's file:line and body; the question is a "choice"
// with the three expected criteria).
func TestAdapter_ClassifyPrior_Resolved(t *testing.T) {
	resp := decide.Response{
		Model: "laya-rl-agent",
		Answers: map[string]decide.Answer{
			"status": {
				Type:             "choice",
				Choice:           "resolved",
				Probabilities:    map[string]float64{"still_valid": 0.01, "resolved": 0.98, "out_of_scope": 0.01},
				Confidence:       0.98,
				AnswerConfidence: 0.98,
				Action:           decide.Action{ActProbability: 1.0},
			},
		},
	}
	baseURL, c := newFakeServer(t, resp)

	client := decide.New(baseURL)
	a := NewAdapter(client, silentLogger())

	dec, err := a.ClassifyPrior(context.Background(), PriorInput{
		File:       "a.go",
		Line:       10,
		PriorBody:  "Missing test",
		DiffAtLine: "@@ ... @@\n+added a test",
	})
	if err != nil {
		t.Fatalf("ClassifyPrior: %v", err)
	}

	if dec.Status != StatusResolved {
		t.Errorf("Status = %q, want %q", dec.Status, StatusResolved)
	}
	if dec.Confidence < 0.95 {
		t.Errorf("Confidence = %f, want >= 0.95", dec.Confidence)
	}
	if got := dec.Probs["resolved"]; got < 0.95 {
		t.Errorf("Probs[resolved] = %f, want >= 0.95", got)
	}

	// Verify the request shape.
	if len(c.stateBodies) != 1 {
		t.Fatalf("got %d state bodies, want 1", len(c.stateBodies))
	}
	state := c.stateBodies[0]
	if !strings.Contains(state, "a.go:10") {
		t.Errorf("state body missing file:line; got %q", state)
	}
	if !strings.Contains(state, "Missing test") {
		t.Errorf("state body missing prior body; got %q", state)
	}
	if !strings.Contains(state, "Diff at file:line") {
		t.Errorf("state body missing diff section; got %q", state)
	}
	if len(c.questions) != 1 {
		t.Fatalf("got %d question sets, want 1", len(c.questions))
	}
	q, ok := c.questions[0]["status"]
	if !ok {
		t.Fatalf("missing 'status' question; got %+v", c.questions[0])
	}
	if q.Type != "choice" {
		t.Errorf("status question type = %q, want choice", q.Type)
	}
	for _, want := range []string{"still_valid", "resolved", "out_of_scope"} {
		if _, ok := q.Criteria[want]; !ok {
			t.Errorf("status criteria missing %q; got %+v", want, q.Criteria)
		}
	}
}

// TestAdapter_ClassifyPrior_StillValid covers the
// "prior is still valid" verdict. The mapping from
// choice to Status should preserve the spelling.
func TestAdapter_ClassifyPrior_StillValid(t *testing.T) {
	resp := decide.Response{
		Model: "laya-rl-agent",
		Answers: map[string]decide.Answer{
			"status": {
				Type:       "choice",
				Choice:     "still_valid",
				Confidence: 0.91,
			},
		},
	}
	baseURL, _ := newFakeServer(t, resp)
	a := NewAdapter(decide.New(baseURL), silentLogger())

	dec, err := a.ClassifyPrior(context.Background(), PriorInput{File: "a.go", Line: 10})
	if err != nil {
		t.Fatalf("ClassifyPrior: %v", err)
	}
	if dec.Status != StatusStillValid {
		t.Errorf("Status = %q, want %q", dec.Status, StatusStillValid)
	}
}

// TestAdapter_ClassifyPrior_OutOfScope covers the
// "no longer relevant" verdict.
func TestAdapter_ClassifyPrior_OutOfScope(t *testing.T) {
	resp := decide.Response{
		Model: "laya-rl-agent",
		Answers: map[string]decide.Answer{
			"status": {
				Type:       "choice",
				Choice:     "out_of_scope",
				Confidence: 0.85,
			},
		},
	}
	baseURL, _ := newFakeServer(t, resp)
	a := NewAdapter(decide.New(baseURL), silentLogger())

	dec, err := a.ClassifyPrior(context.Background(), PriorInput{File: "a.go", Line: 10})
	if err != nil {
		t.Fatalf("ClassifyPrior: %v", err)
	}
	if dec.Status != StatusOutOfScope {
		t.Errorf("Status = %q, want %q", dec.Status, StatusOutOfScope)
	}
}

// TestAdapter_ClassifyPrior_ServerError covers the 5xx
// path. The decide client's retry policy handles 500 by
// default; for the spike we just check the call returns
// an error (not a verdict).
func TestAdapter_ClassifyPrior_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	a := NewAdapter(decide.New(srv.URL), silentLogger())

	_, err := a.ClassifyPrior(context.Background(), PriorInput{File: "a.go", Line: 10})
	if err == nil {
		t.Error("expected error on 500")
	}
}

// TestAdapter_ClassifyPrior_BadRequest covers the 4xx
// path. 4xx is not retried (the retry policy covers
// 429/5xx/transport only).
func TestAdapter_ClassifyPrior_BadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	a := NewAdapter(decide.New(srv.URL), silentLogger())

	_, err := a.ClassifyPrior(context.Background(), PriorInput{File: "a.go", Line: 10})
	if err == nil {
		t.Error("expected error on 400")
	}
}

// TestAdapter_ClassifyPrior_MissingAnswer covers the
// case where the API returns a 200 but no answer for
// the "status" question. The adapter should return an
// error rather than silently returning a zero
// PriorDecision.
func TestAdapter_ClassifyPrior_MissingAnswer(t *testing.T) {
	resp := decide.Response{
		Model:   "laya-rl-agent",
		Answers: map[string]decide.Answer{}, // no answers
	}
	baseURL, _ := newFakeServer(t, resp)
	a := NewAdapter(decide.New(baseURL), silentLogger())

	_, err := a.ClassifyPrior(context.Background(), PriorInput{File: "a.go", Line: 10})
	if err == nil {
		t.Error("expected error when 'status' answer is missing")
	}
}

// TestAdapter_ClassifyPrior_WrongType covers the case
// where the API returns a "noul"-typed answer for the
// "status" question (which is supposed to be a
// "choice"). The adapter should reject the unexpected
// type rather than silently accepting it.
func TestAdapter_ClassifyPrior_WrongType(t *testing.T) {
	resp := decide.Response{
		Model: "laya-rl-agent",
		Answers: map[string]decide.Answer{
			"status": {
				Type: "noul",
				Noul: 0.5,
			},
		},
	}
	baseURL, _ := newFakeServer(t, resp)
	a := NewAdapter(decide.New(baseURL), silentLogger())

	_, err := a.ClassifyPrior(context.Background(), PriorInput{File: "a.go", Line: 10})
	if err == nil {
		t.Error("expected error when 'status' answer is not a choice")
	}
}

// TestBuildStateBody covers the format helper in
// isolation. The orchestrator doesn't have direct
// visibility into the state body format; this is
// sanity coverage.
func TestBuildStateBody(t *testing.T) {
	body := buildStateBody(PriorInput{
		File:       "a.go",
		Line:       10,
		PriorBody:  "Missing test",
		DiffAtLine: "@@ ... @@\n+x",
	})
	for _, want := range []string{"File: a.go:10", "Prior finding:", "Missing test", "Diff at file:line:", "@@ ... @@"} {
		if !strings.Contains(body, want) {
			t.Errorf("state body missing %q; got:\n%s", want, body)
		}
	}
}

// TestBuildStateBody_Empty covers the case where
// PriorBody and DiffAtLine are both empty (the spike
// passes an empty DiffAtLine from the orchestrator
// because no per-prior hunk extraction is wired yet).
func TestBuildStateBody_Empty(t *testing.T) {
	body := buildStateBody(PriorInput{File: "a.go", Line: 10})
	if !strings.Contains(body, "File: a.go:10") {
		t.Errorf("state body missing file:line; got %q", body)
	}
	if strings.Contains(body, "Prior finding:") {
		t.Errorf("state body should not contain 'Prior finding:' when prior body is empty; got %q", body)
	}
	if strings.Contains(body, "Diff at file:line:") {
		t.Errorf("state body should not contain 'Diff at file:line:' when diff is empty; got %q", body)
	}
}

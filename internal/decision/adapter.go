// Package decision contains the adapter that lets the
// orchestrator use a remote decision API (the
// github.com/inful/decide service, which serves the
// Laya decision model) for the per-prior-finding
// classification task.
//
// This is a SPIKE — the adapter is wired through
// Config and called for each prior finding, but the
// LLM verdict is still canonical for the actual
// resolve/keep behaviour. The decision API's verdict
// is logged at debug level for an agreement study
// (the follow-up to the spike will: (a) collect those
// logs into agreement statistics, (b) when agreement
// crosses a threshold, default-enable the override,
// (c) add a confidence threshold below which we fall
// back to the LLM verdict).
//
// The DecisionRunner interface is defined here, not in
// internal/reviewer, so the reviewer package doesn't
// gain a hard dependency on github.com/inful/decide —
// callers wire the adapter in via the Config field.
// Tests use a fake DecisionRunner (in the e2e tests)
// or an httptest-backed decide server (in adapter_test.go).
package decision

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/inful/decide"
)

// Status is the verdict a prior finding can be classified
// into. Mirrors the reviewer's internal FindingStatus but
// is defined here to avoid an import cycle.
type Status string

// Verdict values the decision API can return for a
// prior-finding classification question. The string
// values mirror the orchestrator's FindingStatus
// spellings so the orchestrator can pass them through
// without remapping.
const (
	StatusStillValid Status = "still_valid"
	StatusResolved   Status = "resolved"
	StatusOutOfScope Status = "out_of_scope"
)

// PriorInput is the input to the decision API. The orchestrator
// formats this from the prior's GitLab note + the current
// diff. DiffAtLine is optional in the spike (the orchestrator
// doesn't yet extract per-file:line diff hunks); the body
// alone gives the model enough context to classify the
// 3-way verdict for most priors. A future iteration will
// pass the per-prior hunk here.
type PriorInput struct {
	File       string
	Line       int
	PriorBody  string
	DiffAtLine string
}

// PriorDecision is the output of the decision API.
type PriorDecision struct {
	Status     Status
	Confidence float64
	Probs      map[string]float64
}

// Runner is the interface the orchestrator uses to
// call the decision API. One method, one job — the same
// shape as the existing reviewer.Runner interface (so
// the test surface and the injection pattern are
// consistent). When the orchestrator wants to call the
// decision API, the production wiring passes
// *Adapter here; tests pass a fake.
type Runner interface {
	ClassifyPrior(ctx context.Context, input PriorInput) (PriorDecision, error)
}

// Adapter wraps *decide.Client and adapts the inful/decide
// types to the reviewer's DecisionRunner. The *decide.Client
// carries the base URL, bearer token, retry policy, and
// HTTP client configuration (see cmd/mreview/clients.go
// for the construction site).
type Adapter struct {
	client *decide.Client
	logger *slog.Logger
}

// NewAdapter constructs an Adapter. The client should
// already be configured via decide.New + decide.With*
// options; this constructor does no further setup.
func NewAdapter(client *decide.Client, logger *slog.Logger) *Adapter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{client: client, logger: logger}
}

// ClassifyPrior calls /v1/systemone with a single
// "choice"-typed question (still_valid / resolved /
// out_of_scope) and returns the API's verdict + confidence.
//
// The question's instructions and criteria are the
// authoritative definitions of each status; the API
// returns a `Choice` (the top-class) and `Probabilities`
// (the per-class confidence distribution). The orchestrator
// can threshold on `Confidence` or on `max(Probs)` to
// decide whether the verdict is safe to act on.
func (a *Adapter) ClassifyPrior(ctx context.Context, in PriorInput) (PriorDecision, error) {
	resp, err := a.client.SystemOne(ctx, decide.Request{
		State: decide.State{Body: buildStateBody(in)},
		Questions: decide.Questions{
			"status": decide.NewChoice(
				"Is this prior finding still valid in the current diff, resolved by the new changes, or out of scope for this MR?",
				map[string]string{
					"still_valid":  "the issue is still present in the current diff (or the relevant code is unchanged)",
					"resolved":     "the new changes clearly address the issue (e.g. a test was added, the missing check was added, the bug was fixed)",
					"out_of_scope": "the issue is no longer relevant to this MR (e.g. the file was deleted, the symbol was renamed, the finding referenced code that no longer exists)",
				},
			),
		},
	})
	if err != nil {
		return PriorDecision{}, err
	}
	ans, ok := resp.Answers["status"]
	if !ok {
		return PriorDecision{}, fmt.Errorf("decision API returned no answer for 'status' question")
	}
	if !ans.IsChoice() {
		return PriorDecision{}, fmt.Errorf("decision API returned non-choice answer for 'status' question (type=%q)", ans.Type)
	}
	return PriorDecision{
		Status:     mapChoiceToStatus(ans.ChoiceKey()),
		Confidence: ans.Confidence,
		Probs:      ans.Probabilities,
	}, nil
}

// buildStateBody formats a PriorInput as a string the
// decision API can read. The format is intentionally
// minimal — the API has a small context window (a few
// hundred tokens; the exact limit is the model's
// tolerance, not the API's hard cap).
//
// Spike layout: file:line + prior body + (optional) the
// diff hunk at the file:line. The hunk is empty in the
// current orchestrator (no per-prior hunk extraction
// yet); the model sees the prior body alone, which is
// usually enough for the 3-way verdict. A follow-up
// spike will populate DiffAtLine with the relevant
// diff context for the file:line the prior points at.
func buildStateBody(in PriorInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "File: %s:%d\n", in.File, in.Line)
	if in.PriorBody != "" {
		b.WriteString("\nPrior finding:\n")
		b.WriteString(in.PriorBody)
		b.WriteString("\n")
	}
	if in.DiffAtLine != "" {
		b.WriteString("\nDiff at file:line:\n")
		b.WriteString(in.DiffAtLine)
		b.WriteString("\n")
	}
	return b.String()
}

// mapChoiceToStatus maps the decision API's choice key
// to our internal Status. An unknown choice is returned
// as the raw string so the orchestrator can log it; the
// orchestrator's safe default for unknown statuses is
// still_valid (don't auto-resolve on a verdict we
// can't interpret).
func mapChoiceToStatus(choice string) Status {
	switch choice {
	case "still_valid":
		return StatusStillValid
	case "resolved":
		return StatusResolved
	case "out_of_scope":
		return StatusOutOfScope
	default:
		return Status(choice)
	}
}

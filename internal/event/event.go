// Package event detects the GitLab CI event that triggered this
// mreview invocation and decides whether to proceed with the review
// or skip.
//
// The guard sits at the top of `mreview review` — before any
// GitLab / harness work happens — and short-circuits with a clean
// exit 0 when the event doesn't warrant a review:
//
//   - unsupported CI sources (push, trigger, pipeline, webide, …)
//   - draft merge requests (unless --on-drafts=run)
//   - local CLI invocations (always proceed; treated as "called
//     outside CI" per the issue's local-dev story)
//
// The decision logic is intentionally small. Per-event *content*
// (full vs incremental review, summary vs inline) is still the
// orchestrator's concern; this package only answers the gate
// question "should we run at all for this event?"
//
// Reference: issue #41 ("Per-event review behaviour via GitLab CI
// signals") is the source of truth for the supported-source table.
// Architecture reset #42 calls this guard out as migration step 1.
package event

import (
	"os"
	"strconv"
	"strings"
)

// Source identifies what triggered the GitLab CI pipeline. The
// values match GitLab's CI_PIPELINE_SOURCE predefined variable;
// SourceLocal is the mreview-added sentinel for "no CI env vars
// set" (i.e. an invocation from a developer terminal).
type Source string

const (
	// SourceLocal is the zero value — the binary is running
	// outside CI (no CI_PIPELINE_SOURCE). Always treated as
	// "operator wants a full review."
	SourceLocal Source = ""

	// SourceMergeRequestEvent fires for MR open / reopen / update
	// pipelines. Draft MRs are supported but skipped by default
	// (override with --on-drafts=run).
	SourceMergeRequestEvent Source = "merge_request_event"

	// SourcePush is a direct push to a branch without an MR.
	// Not supported by default (would cause runaway reviews on
	// every commit to main). Opt-in via --on-push=run.
	SourcePush Source = "push"

	// SourceWeb is the manual "Run pipeline" button on the
	// GitLab UI. Full review; no draft check.
	SourceWeb Source = "web"

	// SourceAPI fires when the pipeline is triggered via the
	// GitLab pipelines API. Full review.
	SourceAPI Source = "api"

	// SourceSchedule fires for scheduled (cron) pipelines.
	// Full review.
	SourceSchedule Source = "schedule"

	// SourceTrigger fires for downstream pipelines launched by
	// `trigger:` in another project. Running mreview here would
	// cause feedback loops — not supported.
	SourceTrigger Source = "trigger"

	// SourcePipeline fires for a multi-project pipeline
	// child job. Same feedback-loop concern as SourceTrigger.
	SourcePipeline Source = "pipeline"

	// SourceParentPipeline is the upstream side of a
	// multi-project pipeline parent chain. Not supported.
	SourceParentPipeline Source = "parent_pipeline"

	// SourceWebIDE fires for a pipeline launched from the Web
	// IDE pipeline UI. Not a review-worthy event.
	SourceWebIDE Source = "webide"

	// SourceChat fires for an integration chat-ops command
	// (e.g. Slack "/gitlab run pipeline"). Not supported.
	SourceChat Source = "chat"

	// SourceOnDemandDAST fires for on-demand DAST scans
	// (matches the ondemand_dast_* prefix). Not supported.
	SourceOnDemandDAST Source = "ondemand_dast"

	// SourceSecurityOrchestration fires when a security
	// orchestration policy auto-runs a pipeline. Not
	// supported.
	SourceSecurityOrchestration Source = "security_orchestration_policy"

	// SourceExternalPullRequestEvent fires for an external
	// PR (e.g. GitHub PR mirrored to GitLab). Not supported.
	SourceExternalPullRequestEvent Source = "external_pull_request_event"
)

// Event is the detected CI event that triggered mreview.
//
// MRIID and MRDraft are populated only when Source ==
// SourceMergeRequestEvent; they describe the MR the pipeline
// fired against. The MR's action (open / reopen / update / close
// / merge) is NOT in this struct — it comes from the GitLab API
// at review time, not from the predefined CI variable set.
type Event struct {
	// Source is what triggered the pipeline. SourceLocal when
	// the binary is running outside CI.
	Source Source

	// MRIID is the merge request IID GitLab fired the pipeline
	// against. Zero when Source is not SourceMergeRequestEvent
	// or when CI_MERGE_REQUEST_IID is unset.
	MRIID int

	// MRDraft is true when CI_MERGE_REQUEST_DRAFT == "true".
	// Only meaningful when Source == SourceMergeRequestEvent.
	MRDraft bool

	// FromEnv is true when Source was populated from
	// CI_PIPELINE_SOURCE; false when the binary is running
	// outside CI (Source == SourceLocal). Useful for tests that
	// want to assert "the env was actually read".
	FromEnv bool
}

// Detect reads the GitLab CI environment and returns the event.
// Empty CI_PIPELINE_SOURCE → SourceLocal (the binary is running
// outside CI, treat as a developer-invoked full review).
//
// The detection is intentionally read-only — no env mutation, no
// signal to the parent shell. Tests can swap os.Getenv via the
// package-private getenv hook (see event_test.go).
func Detect() Event {
	e := Event{}
	src := os.Getenv("CI_PIPELINE_SOURCE")
	if src == "" {
		e.Source = SourceLocal
		return e
	}
	e.Source = Source(src)
	e.FromEnv = true

	if iidStr := os.Getenv("CI_MERGE_REQUEST_IID"); iidStr != "" {
		// strconv.Atoi returns an error for non-numeric input;
		// we silently fall back to MRIID=0 (the same as "no env
		// var"). The reviewer will fail later with a clearer
		// error if the IID is malformed.
		if iid, err := strconv.Atoi(iidStr); err == nil {
			e.MRIID = iid
		}
	}
	e.MRDraft = strings.EqualFold(os.Getenv("CI_MERGE_REQUEST_DRAFT"), "true")
	return e
}

// IsSupported reports whether the source is one mreview knows
// how to review without an explicit operator override.
//
// Supported sources:
//   - SourceLocal (developer-invoked)
//   - SourceMergeRequestEvent (draft skipped by default; see Decide)
//   - SourceWeb / SourceAPI / SourceSchedule (manual triggers)
//
// All other sources are "not supported" — see the per-source
// docs on each constant for the rationale.
func (e Event) IsSupported() bool {
	switch e.Source {
	case SourceLocal,
		SourceMergeRequestEvent,
		SourceWeb,
		SourceAPI,
		SourceSchedule:
		return true
	default:
		return false
	}
}

// Package reviewer orchestrates the end-to-end MR review
// pipeline. Migration step 3 of the architecture reset (#42)
// replaces the hand-rolled chunk-loop in the old reviewer.go
// with a harness-driven orchestrator; the orchestrator owns
// the lifecycle:
//
//  1. Fetch the MR + per-file diff from GitLab.
//  2. Chunk the diff via internal/diff.ChunkByFile.
//  3. Render the review prompt (system + user) via
//     internal/prompts.
//  4. Run the harness agent (Runner interface — production
//     wires HarnessRunner).
//  5. Parse the agent's JSON response into findings +
//     summary.
//  6. Apply policy (internal/policy from step 2). The
//     strongest verdict on any finding becomes the exit code
//     (ExitPolicy = 8).
//  7. Dedupe against existing GitLab discussions.
//  8. Post the summary note + inline discussions per
//     finding. Skip on `update` events so pushes don't pile
//     up stale summaries.
//
// The orchestrator owns no goroutines — caller-provided
// context controls cancellation.
package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/inful/mreview/internal/ci/artifact"
	"github.com/inful/mreview/internal/diff"
	"github.com/inful/mreview/internal/git"
	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/policy"
	"github.com/inful/mreview/internal/prompts"
)

// Severity is the verdict the agent assigns to a finding, or
// the policy escalates it to. Same values as policy.Severity;
// duplicated here so this package doesn't pull in policy just
// for type references.
type Severity string

const (
	// SeverityInfo marks a non-blocking comment (nit,
	// suggestion).
	SeverityInfo Severity = "info"

	// SeverityWarning marks a real issue that should be
	// addressed before merge.
	SeverityWarning Severity = "warning"

	// SeverityError marks a blocker (broken behaviour,
	// policy violation).
	SeverityError Severity = "error"
)

// Finding is the shape the orchestrator consumes. Mirrors
// internal/llm.Finding's old shape; the field set is the
// minimum the GitLab inline-discussion payload needs.
type Finding struct {
	File       string   `json:"file"`
	Line       int      `json:"line"`
	Severity   Severity `json:"severity,omitempty"`
	Category   string   `json:"category,omitempty"`
	Body       string   `json:"body"`
	Suggestion string   `json:"suggestion,omitempty"`
}

// ReviewResponse is the JSON envelope the agent emits. Same
// shape the old internal/llm package used, extended with a
// PriorFindings array for the "is each prior finding still
// valid?" judgement (see PriorFindingStatus).
type ReviewResponse struct {
	Findings      []Finding            `json:"findings"`
	PriorFindings []PriorFindingStatus `json:"prior_findings"`
	Summary       string               `json:"summary"`
}

// Runner is what the orchestrator depends on for the LLM
// loop. Production wires HarnessRunner (which wraps
// harness's runtime.Runtime.RunSync); tests use FakeRunner
// to assert orchestrator behaviour without a real LLM.
type Runner interface {
	// RunSync sends the prompt and returns the agent's
	// textual response. Errors propagate; the orchestrator
	// maps them to typed exit codes.
	RunSync(ctx context.Context, system, user string) (string, error)
}

// Config is the orchestrator's constructor input.
type Config struct {
	GitLab      gitlabClient
	Runner      Runner
	WorkDir     string // repository root the harness runs against
	Policy      *policy.Policy
	BotUsername string
	DryRun      bool

	// Artifacts carries the CI artifacts the central pipeline
	// produced before mreview ran (build log, test results,
	// lint, vulns). The orchestrator threads the set into
	// the user prompt so the agent sees them at startup.
	// Nil = no artifacts (dev / local CLI). The render
	// still produces the "all NOT AVAILABLE" block in that
	// case so the agent knows the artifacts are absent
	// rather than just missing.
	Artifacts *artifact.Set

	// DebugLLM, when true, prints the raw response from the
	// LLM (the text RunSync returns, before
	// ParseReviewResponse) to stderr with a clear separator.
	// Independent of --dry-run (which suppresses GitLab
	// side-effects) and --verbose (which raises the slog
	// level; DebugLLM writes raw bytes straight to stderr so
	// multi-line responses stay readable). False by default.
	DebugLLM bool

	// NoDedup disables the same-commit dedup. When false
	// (the default), the orchestrator looks for a prior
	// mreview summary on the MR; if its commit SHA matches
	// the current MR HEAD, the run short-circuits and the
	// LLM is never invoked. When true, every run goes
	// through the full pipeline and posts fresh — useful
	// after a prompt change or when the prior review was
	// wrong and a re-roll is desired. CI that requires an
	// audit trail of every run should set this to true.
	NoDedup bool

	Logger *slog.Logger
}

// Orchestrator is the new review orchestrator. Replaces
// the old Reviewer type; the migration is "drop the old,
// build the new, swap the call site."
type Orchestrator struct {
	cfg    Config
	logger *slog.Logger
}

// New builds the orchestrator. The Runner is required; nil
// is rejected.
func New(cfg Config) (*Orchestrator, error) {
	if cfg.GitLab == nil {
		return nil, errors.New("reviewer: Config.GitLab is required")
	}
	if cfg.Runner == nil {
		return nil, errors.New("reviewer: Config.Runner is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Orchestrator{cfg: cfg, logger: cfg.Logger}, nil
}

// Result is the orchestrator's return value. Same shape the
// old Reviewer.Result had; the GitLab layer that consumes
// this hasn't changed (it's still the postDiscussion +
// postSummary plumbing).
type Result struct {
	MR          *gitlab.MergeRequest
	Findings    []PostedFinding
	Summary     *gitlab.Note
	DedupeSize  int
	PolicyError bool // any error-verdict finding → exit 8
	PolicyWarn  bool

	// SkippedReason is non-empty when the orchestrator
	// short-circuited and didn't run the LLM. Today only one
	// path produces a skip: a prior mreview summary on the MR
	// already covers the current commit (see dedup logic in
	// Run). Future skip paths (e.g. dry-run-only mode) may
	// also set this.
	SkippedReason string
}

// PostedFinding records one inline-discussion post (or a
// skipped one) so the caller can log the outcome.
type PostedFinding struct {
	Finding Finding
	Skipped bool
	Reason  string
}

// Run executes the full review pipeline for one MR. The
// optional action parameter carries the GitLab MR action
// (open / reopen / update / close / merge) — passed only
// when the orchestrator runs from a webhook context. Empty
// means "called from the CLI directly" and behaves like
// `open`.
// Run executes the full review pipeline for one MR. project
// is either a string (GitLab URL slug, e.g. "group/project") or
// an int (numeric project ID); both are accepted by the
// underlying client-go SDK. In CI, the numeric ID is
// preferred because it sidesteps URL-encoding issues that
// some self-hosted NGINX configs have with the project path.
//
// The optional action parameter carries the GitLab MR action
// (open / reopen / update / close / merge) — passed only when
// the orchestrator runs from a webhook context. Empty means
// "called from the CLI directly" and behaves like `open`.
//
// The action is currently not branched on — the orchestrator
// always processes the MR the same way regardless of
// event type. Earlier code skipped the summary post on
// "update" (push) events; that skip was removed when the
// "edit existing summary in place" behaviour landed
// (the PUT-based edit doesn't bump the activity feed the
// way POST did, so the spam concern went away). The
// `action` parameter is kept on the signature for future
// event-aware behaviour and to preserve the public API.
func (o *Orchestrator) Run(ctx context.Context, project any, iid int, action ...string) (*Result, error) {
	_ = action // intentionally ignored today; see comment above

	logger := o.logger.With("run", "review", "project", project, "mr_iid", iid)

	mr, err := o.cfg.GitLab.FetchMR(ctx, project, iid)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: fetch MR: %w", err)
	}
	logger.Info("fetched MR", "title", mr.Title, "state", mr.State, "sha", mr.SHA)

	// Dedup by commit: before any LLM work, look for a prior
	// mreview summary on this MR. If we find one with the
	// same commit SHA, this run is a no-op — the prior
	// review already represents this exact commit, and
	// re-running would burn tokens for no new information.
	//
	// If we find one with a DIFFERENT SHA, the source branch
	// has new commits since the prior run. We extract the
	// prior findings and pass them to the LLM as context;
	// the LLM is asked to mirror them in its `prior_findings`
	// response with a status (still_valid / resolved /
	// out_of_scope), which the orchestrator uses to decide
	// which prior discussions to keep open vs. auto-resolve.
	// The summary is EDITED in place (not posted fresh) so
	// the activity feed shows one mreview note per MR that
	// evolves over time, not a pile of stale summaries.
	//
	// Skipped by --no-dedup (operator override: force a fresh
	// run regardless, e.g. after a prompt change or when the
	// prior review was wrong and a re-roll is desired).
	//
	// The `prior` variable is also reused below as the
	// "summary to edit" target when a prior exists; we
	// capture it here so the summary-edit path at the
	// bottom of Run doesn't need a second ListDiscussions
	// call.
	//
	// We fetch discussions ONCE up front and pass the list
	// to both findPriorSummary (commit-level dedup) and
	// later to findPriorFindingLocations (file:line dedup
	// of new findings vs. prior) — three consumers, one
	// network call.
	var discs []gitlab.Discussion
	var prior *PriorReview
	var priorFindings []PriorFinding
	if !o.cfg.NoDedup && mr.SHA != "" {
		var derr error
		discs, derr = o.cfg.GitLab.ListDiscussions(ctx, project, iid)
		if derr != nil {
			logger.Debug("dedup pre-check failed; proceeding with fresh review",
				"err", derr.Error(),
			)
		} else {
			prior = FindPriorMReviewSummary(discs)
			logger.Debug("dedup: scanned existing discussions",
				"project", project, "iid", iid, "discussions", len(discs),
			)
		}
		if prior != nil {
			if prior.Commit == mr.SHA {
				logger.Info("review skipped: already reviewed this commit",
					"commit", mr.SHA,
					"prior_summary_id", prior.SummaryDiscussionID,
					"prior_findings", len(prior.FindingDiscussionIDs),
				)
				return &Result{
					MR:            mr,
					SkippedReason: "already reviewed this commit",
				}, nil
			}
			// Different commit → run a fresh review AND
			// extract the prior findings so the LLM can
			// judge which are still valid vs. resolved by
			// the new changes. The prior summary will be
			// edited in place (not posted fresh) at the
			// bottom of Run; the prior findings will be
			// auto-resolved per the LLM's verdict.
			//
			// We use the same `discs` list for both the
			// prior-summary lookup and the prior-findings
			// extraction, so the dedup pre-check above
			// serves as a free prior-findings data fetch.
			priorFindings = extractPriorFindings(prior, discs, o.cfg.BotUsername)
			logger.Info("dedup: fresh review will run; prior findings extracted for LLM context",
				"prior_commit", prior.Commit,
				"new_commit", mr.SHA,
				"prior_summary_id", prior.SummaryDiscussionID,
				"prior_findings_total", len(prior.FindingDiscussionIDs),
				"prior_findings_authored", len(priorFindings),
			)
		}
	}

	// Branch sanity-check: if the workdir is checked out on a
	// different branch than the MR's source branch, tokensave
	// will index (and serve) the wrong tree. The agent's
	// tokensave_context / tokensave_search / tokensave_read
	// calls will then return data from a different revision
	// than the diff it's reviewing — the agent can't reconcile,
	// retries, and burns the 8192-token output budget. This
	// mismatch is easy to hit in local dev where the operator
	// forgets to checkout the MR's source branch; in CI, the
	// central pipeline checks out the MR branch before
	// invoking mreview, so the mismatch is rare but possible
	// (e.g. when reused workdirs from earlier runs survive).
	//
	// The check shells out to `git`. The debug Docker image
	// (cmd/mreview/Dockerfile.debug) bundles git at
	// /usr/local/bin/git — the example GitLab CI template
	// (examples/gitlab-ci.yml) uses :latest-debug, so this
	// WARN fires for real in CI. The production image
	// (:latest) does NOT bundle git and degrades silently
	// to a debug log via the ErrNoGit path below; local dev
	// (where git is on PATH) always gets the loud WARN.
	//
	// We intentionally DO NOT auto-checkout. Run-time git
	// mutation from a code-review CLI is surprising, can
	// destroy uncommitted work, and would force the operator
	// to trust our run-script hygiene. The fix is one shell
	// line for the operator.
	if o.cfg.WorkDir != "" && mr.SourceBranch != "" {
		wcur, werr := git.CurrentBranch(o.cfg.WorkDir)
		switch {
		case errors.Is(werr, git.ErrNoGit):
			logger.Debug("skipping branch check: git not on PATH (production :latest image, or a non-debug local dev)")
		case errors.Is(werr, git.ErrNotARepo):
			logger.Debug("skipping branch check: workdir is not a git repository",
				"workdir", o.cfg.WorkDir,
			)
		case werr != nil:
			logger.Debug("branch check failed",
				"workdir", o.cfg.WorkDir,
				"err", werr.Error(),
			)
		case wcur == "":
			// Detached HEAD or uncommitted state — not a
			// "branch mismatch"; leave the operator alone.
			logger.Debug("workdir has no current branch (detached HEAD?)",
				"workdir", o.cfg.WorkDir,
			)
		case wcur != mr.SourceBranch:
			logger.Warn("workdir branch does not match MR source branch — tokensave will index and serve the wrong tree",
				"workdir", o.cfg.WorkDir,
				"workdir_branch", wcur,
				"mr_source_branch", mr.SourceBranch,
				"reason", "tokensave serve defaults to the workdir's checked-out branch; running the review against the wrong tree makes the agent's tokensave_* calls return data from a different revision than the diff",
				"remediation", fmt.Sprintf("git -C %s checkout %s", o.cfg.WorkDir, mr.SourceBranch),
			)
		}

		// Pre-track the MR's source branch in tokensave.
		// Without this, every tokensave tool call appends
		// "WARNING: branch 'X' is not tracked — serving from
		// 'Y'" to its response, which is ~150 chars of
		// pure noise per call. Across a 6-call review
		// that's ~900 tokens eaten out of every per-call
		// output budget. tokensave's `branch add` is
		// idempotent (re-tracking is a no-op incremental
		// sync), so doing this on every run is cheap.
		// Failure modes degrade gracefully to a debug log
		// (see internal/git for the sentinel errors).
		if terr := git.EnsureBranchTracked(o.cfg.WorkDir, mr.SourceBranch); terr != nil {
			switch {
			case errors.Is(terr, git.ErrNoTokensave):
				logger.Debug("tokensave branch pre-track skipped: tokensave not on PATH")
			default:
				logger.Debug("tokensave branch pre-track failed",
					"workdir", o.cfg.WorkDir,
					"branch", mr.SourceBranch,
					"err", terr.Error(),
				)
			}
		}
	}

	changes, err := o.cfg.GitLab.FetchChanges(ctx, project, iid)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: fetch changes: %w", err)
	}
	logger.Info("fetched changes", "files", len(changes))

	// Chunk via the diff package. ChangeFile satisfies
	// diff.Source via the changeFileSource adapter (defined
	// in this package).
	srcs := make([]diff.Source, len(changes))
	for i := range changes {
		srcs[i] = changeFileSource{cf: &changes[i]}
	}

	chunks, err := diff.ChunkByFile(srcs, 200_000) // 200KB default; PR #5 will wire up the operator flag
	if err != nil {
		return nil, fmt.Errorf("orchestrator: chunk diff: %w", err)
	}
	logger.Info("chunked diff", "chunks", len(chunks))

	// Build the prompt.
	meta := prompts.ReviewMetadata{
		IID:          int64(mr.IID),
		Title:        mr.Title,
		Description:  mr.Description,
		Author:       mr.Author.Username,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
	}
	artifactSet := o.cfg.Artifacts
	if artifactSet == nil {
		// Empty set — the prompt still renders the artifact
		// block with all "NOT AVAILABLE" markers so the agent
		// can distinguish "no artifacts configured" from
		// "missing artifacts that should have been here".
		artifactSet = &artifact.Set{}
	}
	// Convert the reviewer's PriorFinding (which carries
	// GitLab plumbing) to the prompts-package PriorFinding
	// (which doesn't import internal/gitlab). The
	// conversion drops the GitLab IDs — the LLM doesn't
	// need them, the orchestrator keeps them on the
	// reviewer-side copy for resolve-after-the-fact.
	promptPrior := make([]prompts.PriorFinding, len(priorFindings))
	for i, pf := range priorFindings {
		promptPrior[i] = prompts.PriorFinding{
			File:       pf.File,
			Line:       pf.Line,
			Severity:   "",
			Category:   "",
			Body:       pf.Body,
			Suggestion: pf.Suggestion,
		}
	}
	userPrompt := prompts.ReviewUserPrompt(meta, chunks, *artifactSet, promptPrior)

	// Run the harness agent.
	logger.Info("running review agent", "system_prompt_bytes", len(prompts.ReviewSystemPrompt()), "user_prompt_bytes", len(userPrompt))
	raw, err := o.cfg.Runner.RunSync(ctx, prompts.ReviewSystemPrompt(), userPrompt)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: runner: %w", err)
	}

	// Optional: dump the raw LLM response to stderr so an
	// operator can see exactly what the model emitted when
	// the parser rejects it. Independent of the slog level
	// (this is meant for raw bytes, not structured logging)
	// and of --dry-run (which suppresses downstream effects).
	if o.cfg.DebugLLM {
		body := raw
		if body == "" {
			body = "(empty response)"
		}
		fmt.Fprintf(os.Stderr,
			"\n=== mreview --debug-llm: raw LLM response (mr=%d, %d bytes) ===\n%s\n=== end --debug-llm ===\n",
			mr.IID, len(raw), body,
		)
	}

	// Parse the response.
	resp, err := ParseReviewResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: parse: %w", err)
	}

	// Apply policy.
	policyInput := policy.Input{
		Findings: toPolicyFindings(resp.Findings),
	}
	policyRes := o.applyPolicy(policyInput)
	logger.Info("policy applied",
		"findings", len(policyRes.Findings),
		"synthetics", len(policyRes.Synthetics),
		"has_error", policyRes.HasError,
	)

	// Dedupe against existing discussions. The dedup gate is
	// file:line against UNRESOLVED prior findings — see the
	// comment on file_line_dedup.go for the rationale. A
	// prior finding that the operator already resolved (or
	// that an earlier mreview run marked obsolete) is NOT in
	// the set; we want the LLM to be free to surface a new
	// finding at the same location if one really exists.
	//
	// If we already fetched `discs` above (for the
	// commit-level dedup), reuse that list. Otherwise
	// (NoDedup set, or the prior-fetch failed) make the
	// call now. Either way, the same `discs` list is also
	// used later to resolve the prior findings the LLM
	// marks resolved / out_of_scope.
	if discs == nil {
		var derr error
		discs, derr = o.cfg.GitLab.ListDiscussions(ctx, project, iid)
		if derr != nil {
			logger.Warn("dedupe fetch failed; proceeding without dedupe", "err", derr.Error())
			discs = nil
		}
	}
	priorLocs := findPriorFindingLocations(discs, o.cfg.BotUsername)
	logger.Info("dedupe set built (file:line, unresolved prior)",
		"size", priorLocs.Size(),
	)
	toPost := dedupFindingsByFileLine(policyRes.Findings, priorLocs, logger)

	result := &Result{
		MR:          mr,
		Findings:    make([]PostedFinding, 0, len(toPost)),
		DedupeSize:  priorLocs.Size(),
		PolicyError: policyRes.HasError,
		PolicyWarn:  policyRes.HasWarning,
	}

	// Process the LLM's `prior_findings` response: classify
	// each prior finding as keep (still_valid), resolve
	// (resolved), or resolve+out-of-scope (out_of_scope).
	// Anything the LLM didn't mention is treated as
	// still_valid (the safe default — the LLM's silence
	// shouldn't trigger an auto-resolve the operator didn't
	// ask for).
	priorActions := classifyPriorFindings(resp.PriorFindings, priorFindings, logger)

	// Auto-resolve the prior findings the LLM marked
	// resolved or out_of_scope. Best-effort: a 404 (the
	// discussion was already deleted out-of-band) or any
	// other error logs at warn and moves on; the rest of
	// the review still posts. Done BEFORE the summary
	// post/edit so the resolve threads collapse in the UI
	// by the time the new summary lands.
	if !o.cfg.DryRun {
		for _, pf := range priorActions.resolve {
			// ResolveDiscussion takes a string project,
			// not the any-typed project the rest of the
			// orchestrator uses. Convert here so the
			// GitLab API can resolve a discussion
			// regardless of whether the caller passed
			// a project slug or a numeric ID.
			projStr, ok := project.(string)
			if !ok {
				logger.Warn("prior-finding resolve skipped: project must be a string slug, got %T", project)
				continue
			}
			if err := o.cfg.GitLab.ResolveDiscussion(ctx, projStr, iid, pf.DiscussionID); err != nil {
				logger.Warn("prior-finding resolve failed",
					"discussion_id", pf.DiscussionID,
					"file", pf.File, "line", pf.Line,
					"err", err.Error(),
				)
			}
		}
	}

	// Build the unified rows for the summary table:
	//   1. New findings (post this run)
	//   2. Carried-over prior findings (the LLM said still_valid)
	//   3. Resolved prior findings (the LLM said resolved or
	//      out_of_scope; included for the operator's history)
	// Display order: new first, then carried-over, then
	// resolved. Within each group, sort by file:line for
	// stable rendering across runs.
	resolvedEntries := make([]priorResolvedEntry, len(priorActions.resolve))
	for i, pf := range priorActions.resolve {
		resolvedEntries[i] = priorResolvedEntry{
			Finding:   pf,
			Status:    priorActions.resolveStatuses[i],
			Rationale: priorActions.resolveRationale[i],
		}
	}
	rows := buildSummaryRows(toPost, priorActions.carryOver, resolvedEntries)

	// Order: post inline findings FIRST (so we have the
	// new discussion IDs), then write the summary ONCE
	// (with the full marker). The previous "post + edit
	// fill-in" pattern required two round-trips because
	// the post carried a commit-only marker; the new
	// "post-findings + single-write" pattern is one
	// round-trip, since we already know the carried-over
	// IDs (from the prior summary's
	// FindingDiscussionIDs) and can collect the new IDs
	// in the same loop that posts the findings.
	//
	// For the prior-edit case: the single write is an
	// EditSummary (PUT) of the prior note with the new
	// body. For the first-run case: it's a PostSummary
	// of a fresh note. Either way, exactly one summary
	// round-trip.
	//
	// The previous `act == "update"` skip (which suppressed
	// the summary write on push events) has been removed:
	// the operator wants the summary to always reflect the
	// current state, and the PUT-based edit doesn't bump
	// the activity feed the way POST did, so the "fresh
	// post on every push" spam is gone.
	var postedFindingIDs []string
	for _, f := range toPost {
		meta := changeMetaFor(pathIndex(changes), f.File)
		cmt := buildInlineComment(meta, f)
		if o.cfg.DryRun {
			result.Findings = append(result.Findings, PostedFinding{Finding: f, Skipped: true, Reason: "dry-run"})
			continue
		}
		disc, err := o.cfg.GitLab.PostDiscussion(ctx, project, iid, mr.DiffRefs, cmt)
		if err != nil {
			logger.Warn("inline post failed", "file", f.File, "line", f.Line, "err", err.Error())
			result.Findings = append(result.Findings, PostedFinding{Finding: f, Skipped: true, Reason: err.Error()})
			continue
		}
		// Capture the discussion ID so the summary
		// marker (written below) can reference it.
		// Without this, the next mreview run can
		// detect "prior summary at this SHA" but cannot
		// resolve the prior inline findings when the
		// SHA advances.
		if disc != nil {
			postedFindingIDs = append(postedFindingIDs, disc.ID)
		}
		result.Findings = append(result.Findings, PostedFinding{Finding: f})
	}

	// Marker finding-IDs: the next run's "prior findings"
	// are the inline discussions still open after this
	// run, i.e. the new IDs we just posted + the
	// carried-over IDs (the resolved IDs are
	// auto-collapsed, so the next run's prior-set
	// shouldn't see them as still-open).
	markerFindingIDs := append(priorActions.carryOverIDs, postedFindingIDs...)

	// Write the summary: edit-in-place when a prior
	// exists, post-fresh otherwise. One round-trip in
	// either case. Skipped on dry-run (no findings
	// were posted, so the body would be empty; the
	// dry-run path is for testing the orchestrator
	// without side effects, not for producing a real
	// summary).
	if !o.cfg.DryRun {
		body := renderSummary(mr, resp.Summary, rows, markerFindingIDs)
		if prior != nil && prior.SummaryDiscussionID != "" {
			// Edit in place. We need the note ID (not
			// the discussion ID); look it up from the
			// prior discussion's first note.
			priorNoteID := int64(0)
			for _, d := range discs {
				if d.ID == prior.SummaryDiscussionID && len(d.Notes) > 0 {
					priorNoteID = d.Notes[0].ID
					break
				}
			}
			if priorNoteID > 0 {
				edited, err := o.cfg.GitLab.EditSummary(ctx, project, iid, priorNoteID, body)
				if err != nil {
					logger.Warn("summary edit failed", "err", err.Error())
				} else {
					result.Summary = edited
				}
			} else {
				logger.Warn("summary edit skipped: prior note ID not found",
					"prior_summary_id", prior.SummaryDiscussionID,
				)
			}
		} else {
			note, err := o.cfg.GitLab.PostSummary(ctx, project, iid, body)
			if err != nil {
				logger.Warn("post summary failed", "err", err.Error())
			} else {
				result.Summary = note
			}
		}
	}

	logger.Info("review complete",
		"new_findings_posted", len(postedFindingIDs),
		"prior_findings_carried_over", len(priorActions.carryOver),
		"prior_findings_resolved", len(priorActions.resolve),
	)

	return result, nil
}

// applyPolicy runs the loaded policy against the agent's
// findings. Nil-safe: nil policy → pass-through with the
// agent's verdict as the final verdict.
func (o *Orchestrator) applyPolicy(in policy.Input) policy.Result {
	if o.cfg.Policy == nil {
		// No policy: pass through with agent-assigned
		// severity as the verdict.
		res := policy.Result{}
		for _, f := range in.Findings {
			res.Findings = append(res.Findings, policy.EnforcedFinding{
				File:     f.File,
				Line:     f.Line,
				Severity: f.Severity,
				Category: f.Category,
				Body:     f.Body,
				Verdict:  f.Severity,
				Reason:   "llm",
			})
			switch f.Severity {
			case policy.SeverityError:
				res.HasError = true
			case policy.SeverityWarning:
				res.HasWarning = true
			case policy.SeverityInfo:
				res.HasInfo = true
			}
		}
		return res
	}
	return o.cfg.Policy.Enforce(in)
}

// toPolicyFindings converts reviewer.Finding to policy.Finding.
// Lives here (not in the policy package) so the policy package
// doesn't import this package.
func toPolicyFindings(in []Finding) []policy.Finding {
	out := make([]policy.Finding, len(in))
	for i, f := range in {
		out[i] = policy.Finding{
			File:     f.File,
			Line:     f.Line,
			Severity: policy.Severity(f.Severity),
			Category: f.Category,
			Body:     f.Body,
		}
	}
	return out
}

// ParseReviewResponse extracts a ReviewResponse from the
// agent's raw text reply. Same layered strategy as the old
// internal/llm/parse.go (raw → fenced → loose bracket →
// streaming) but lives here because the package owns the
// Finding / ReviewResponse types now.
//
// For brevity in this migration, only the raw + fenced
// strategies are implemented here; the streaming fallback
// can land in a follow-up if local models still produce
// truncated output.
func ParseReviewResponse(raw string) (ReviewResponse, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ReviewResponse{}, errors.New("parse: empty response")
	}
	// Strip a single fence pair if present.
	body := trimmed
	if strings.HasPrefix(body, "```") {
		body = stripFences(body)
	}
	var resp ReviewResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return ReviewResponse{}, fmt.Errorf("parse: %w", err)
	}
	return resp, nil
}

// stripFences removes the outer ```...``` (or ```json ...```)
// wrapper if the response starts and ends with one. Doesn't
// try to be clever — it just trims the first and last lines
// when they look like fences. An unclosed fence (open with
// no close) is left intact so the JSON parser can fail
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

// changeFileSource wraps gitlab.ChangeFile to satisfy
// diff.Source. The wrapper lives here (not in the gitlab
// package) so ChangeFile stays field-shaped for JSON
// unmarshalling. See internal/diff/chunk.go for the Source
// interface.
type changeFileSource struct{ cf *gitlab.ChangeFile }

func (s changeFileSource) Diff() string    { return s.cf.Diff }
func (s changeFileSource) OldPath() string { return s.cf.OldPath }
func (s changeFileSource) NewPath() string { return s.cf.NewPath }
func (s changeFileSource) IsNew() bool     { return s.cf.NewFile }
func (s changeFileSource) IsDeleted() bool { return s.cf.DeletedFile }
func (s changeFileSource) IsRenamed() bool { return s.cf.RenamedFile }
func (s changeFileSource) Path() string    { return s.cf.Path() }

// changeMetaFor looks up the change metadata for file in
// changes. Returns zero-value when not found (defensive —
// should not happen post-filter).
func changeMetaFor(index map[string]gitlab.ChangeFile, file string) gitlab.ChangeFile {
	if v, ok := index[file]; ok {
		return v
	}
	return gitlab.ChangeFile{}
}

// pathIndex builds a path → ChangeFile map for quick lookups
// during inline-comment construction.
func pathIndex(changes []gitlab.ChangeFile) map[string]gitlab.ChangeFile {
	out := make(map[string]gitlab.ChangeFile, len(changes))
	for _, c := range changes {
		out[c.Path()] = c
	}
	return out
}

// buildInlineComment constructs the InlineComment for one
// finding. Same logic as the old internal/reviewer/post.go.
func buildInlineComment(meta gitlab.ChangeFile, f Finding) gitlab.InlineComment {
	cmt := gitlab.InlineComment{
		File:       f.File,
		Body:       f.Body,
		Suggestion: f.Suggestion,
	}

	switch {
	case meta.NewFile:
		cmt.NewLine = f.Line
	case meta.DeletedFile:
		cmt.OldPath = meta.OldPath
		cmt.OldLine = f.Line
	default:
		cmt.NewLine = f.Line
		if meta.OldPath != "" && meta.OldPath != meta.NewPath {
			cmt.OldPath = meta.OldPath
		}
	}
	return cmt
}

// SummaryRow is one entry in the findings table. Carries
// everything the renderer needs to draw a row for any of
// the three categories (new / carried-over / resolved).
//
// The orchestrator builds SummaryRow values from the LLM's
// response: new findings from resp.Findings, carried-over
// and resolved rows from resp.PriorFindings joined with
// the prior-finding data (file, line, body, GitLab
// discussion/note IDs). The Status field drives both the
// Status column's emoji and the row's display position.
type SummaryRow struct {
	File      string
	Line      int
	Severity  string // LLM-assigned for new; "(prior)" for carried-over / resolved
	Category  string // LLM-assigned for new; "(prior)" for carried-over / resolved
	Body      string
	Status    FindingStatus
	Rationale string // resolved / out_of_scope only; suffixed to body
}

// renderSummary is the human-readable verdict the orchestrator
// posts (or edits in place) as a single MR-level note. Kept
// here for now; a future PR may move it into internal/prompts
// for templating parity with the system prompt.
//
// The Findings section is rendered as a Markdown table
// (Status, Severity, File, Line, Category, Description) with
// each row's status driving a leading emoji. The Status
// column is the new piece: it tells the operator at a
// glance which findings are new since the last run, which
// were carried over (still valid in the new diff), and
// which were resolved by the new changes. A table rather
// than a bullet list (the older form) so each finding
// keeps its own line in GitLab's rendering and multi-line
// bodies stay readable via in-cell `<br>` separators.
//
// The whole table is wrapped in GitLab-Flavored Markdown's
// <details> collapsible block, with the row count in the
// <summary>. Default GitLab rendering collapses the body of
// the comment to a single click-to-expand row, which keeps
// the human-readable summary at top without a long table
// pushing it off-screen on MRs with many findings. Reviewers
// who want the table click the summary; the summary itself
// is always visible.
//
// Body escaping: pipe characters in the body would otherwise
// terminate the table row; backslash-escape them. Newlines
// inside bodies become `<br>` so each sentence keeps its
// own visual line inside the table cell.
//
// Dedup marker: when mr.SHA is non-empty, the body is
// preceded by a hidden HTML comment carrying the commit SHA
// and the inline-finding discussion IDs that this run
// considers "live" (new + carried-over — resolved findings
// are auto-collapsed and don't need to be tracked by the
// next run). The next mreview run on this MR reads the
// marker to (1) skip if the SHA matches, or (2) extract the
// prior findings for the LLM to evaluate if the SHA
// differs. The marker is invisible in GitLab's rendered
// view. See marker.go (FormatMarkerLine / ParseMarkerLine)
// for the wire format.
func renderSummary(mr *gitlab.MergeRequest, summary string, rows []SummaryRow, findingIDs []string) string {
	var b strings.Builder
	if mr != nil && mr.SHA != "" {
		b.WriteString(FormatMarkerLine(mr.SHA, findingIDs))
		b.WriteString("\n")
	}
	b.WriteString("# mreview summary\n\n")
	if summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}
	// Wrap the findings in <details> so the table collapses
	// by default. The blank line between <summary> and the
	// table is required — GitLab's markdown parser only
	// recognises a markdown table when it follows an empty
	// line, even inside an HTML block.
	fmt.Fprintf(&b, "<details>\n<summary>Findings (%d)</summary>\n\n", len(rows))
	if len(rows) == 0 {
		b.WriteString("No issues found.\n")
	} else {
		b.WriteString("| Status | Severity | File | Line | Category | Description |\n")
		b.WriteString("|--------|----------|------|-----:|----------|-------------|\n")
		for _, r := range rows {
			// Severity column: LLM-assigned severity with
			// a leading emoji (🛑 error / ⚠️ warning /
			// ℹ️ info) for new findings, "(prior)"
			// placeholder for carried-over and resolved.
			// The carried-over / resolved rows show the
			// original finding body (from the GitLab
			// note), not a re-rendered severity — the
			// original severity is visible by clicking
			// through to the (open or auto-resolved)
			// discussion.
			severity := r.Severity
			severityPrefix := ""
			if r.Status == StatusStillValid || r.Status == StatusResolved || r.Status == StatusOutOfScope {
				severity = "(prior)"
			} else {
				severityPrefix = severityEmoji(r.Severity) + " "
			}
			category := r.Category
			if r.Status == StatusStillValid || r.Status == StatusResolved || r.Status == StatusOutOfScope {
				category = "(prior)"
			}
			body := strings.TrimSpace(r.Body)
			// For resolved / out_of_scope, append the LLM's
			// rationale so the operator can see WHY the
			// prior finding was marked resolved (without
			// clicking through to the now-collapsed thread).
			if r.Rationale != "" && (r.Status == StatusResolved || r.Status == StatusOutOfScope) {
				body = body + "<br><em>Resolved: " + strings.TrimSpace(r.Rationale) + "</em>"
			}
			// Markdown tables render newlines as a literal space
			// inside a cell; <br> is the documented GitLab-Flavored
			// Markdown escape for an in-cell line break.
			body = strings.ReplaceAll(body, "\r\n", "<br>")
			body = strings.ReplaceAll(body, "\n", "<br>")
			// Pipe would terminate the row; backslash-escape.
			body = strings.ReplaceAll(body, "|", `\|`)
			fmt.Fprintf(&b, "| %s %s | %s%s | `%s` | %d | %s | %s |\n",
				r.Status.Emoji(), r.Status.DisplayLabel(),
				severityPrefix, severity, r.File, r.Line, category, body)
		}
	}
	// Trailing blank line then </details> — same reason as
	// the leading blank: GitLab's HTML/Markdown boundary
	// rules want whitespace there.
	b.WriteString("\n</details>\n")
	return b.String()
}

// harnessRunner is the production Runner — wraps
// harness's runtime.Runtime. Defined in a separate file so
// this file stays focused on the orchestrator pipeline.
var _ Runner = (*harnessRunner)(nil)

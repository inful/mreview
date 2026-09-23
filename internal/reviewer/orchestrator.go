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
	"strings"

	"github.com/inful/mreview/internal/ci/artifact"
	"github.com/inful/mreview/internal/diff"
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
// shape the old internal/llm package used.
type ReviewResponse struct {
	Findings []Finding `json:"findings"`
	Summary  string    `json:"summary"`
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
	GitLab      *gitlab.Client
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
func (o *Orchestrator) Run(ctx context.Context, project string, iid int, action ...string) (*Result, error) {
	act := ""
	if len(action) > 0 {
		act = action[0]
	}

	logger := o.logger.With("run", "review", "project", project, "mr_iid", iid)

	mr, err := o.cfg.GitLab.FetchMR(ctx, project, iid)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: fetch MR: %w", err)
	}
	logger.Info("fetched MR", "title", mr.Title, "state", mr.State)

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
	userPrompt := prompts.ReviewUserPrompt(meta, chunks, *artifactSet)

	// Run the harness agent.
	logger.Info("running review agent", "system_prompt_bytes", len(prompts.ReviewSystemPrompt()), "user_prompt_bytes", len(userPrompt))
	raw, err := o.cfg.Runner.RunSync(ctx, prompts.ReviewSystemPrompt(), userPrompt)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: runner: %w", err)
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

	// Dedupe against existing discussions.
	discs, err := o.cfg.GitLab.ListDiscussions(ctx, project, iid)
	if err != nil {
		logger.Warn("dedupe fetch failed; proceeding without dedupe", "err", err.Error())
		discs = nil
	}
	fpSet := newFingerprintSet(discs, o.cfg.BotUsername)
	logger.Info("dedupe set built", "size", fpSet.Size())
	// Convert EnforcedFinding → Finding for the dedupe + post
	// stages (the dedupe fingerprints Body, the GitLab post
	// builds an InlineComment from the local Finding fields).
	toPost := enforcedToLocal(policyRes.Findings)
	if fpSet.Size() > 0 {
		toPost = dedupeAgainstSet(policyRes.Findings, fpSet, logger)
	}

	result := &Result{
		MR:          mr,
		Findings:    make([]PostedFinding, 0, len(toPost)),
		DedupeSize:  fpSet.Size(),
		PolicyError: policyRes.HasError,
		PolicyWarn:  policyRes.HasWarning,
	}

	// Post the summary (skip on update events).
	if !o.cfg.DryRun && act != "update" {
		body := renderSummary(mr, resp.Summary, policyRes.Findings)
		if body != "" {
			note, err := o.cfg.GitLab.PostSummary(ctx, project, iid, body)
			if err != nil {
				logger.Warn("post summary failed", "err", err.Error())
			} else {
				result.Summary = note
			}
		}
	}

	// Post inline findings.
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
		_ = disc
		result.Findings = append(result.Findings, PostedFinding{Finding: f})
	}

	return result, nil
}

// enforcedToLocal converts policy.EnforcedFinding back to the
// local Finding type (preserves Body, File, Line, Category;
// uses Verdict as Severity so the GitLab post uses the
// post-policy severity).
func enforcedToLocal(in []policy.EnforcedFinding) []Finding {
	out := make([]Finding, len(in))
	for i, f := range in {
		out[i] = Finding{
			File:     f.File,
			Line:     f.Line,
			Severity: Severity(f.Verdict),
			Category: f.Category,
			Body:     f.Body,
		}
	}
	return out
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

// renderSummary is the human-readable verdict the orchestrator
// posts as a single MR-level note. Kept here for now; a future
// PR may move it into internal/prompts for templating parity
// with the system prompt.
func renderSummary(mr *gitlab.MergeRequest, summary string, findings []policy.EnforcedFinding) string {
	var b strings.Builder
	b.WriteString("# mreview summary\n\n")
	if summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}
	b.WriteString("## Findings\n\n")
	if len(findings) == 0 {
		b.WriteString("No issues found.\n")
		return b.String()
	}
	for _, f := range findings {
		emoji := "•"
		switch policy.Severity(f.Verdict) {
		case policy.SeverityError:
			emoji = "🛑"
		case policy.SeverityWarning:
			emoji = "⚠️"
		case policy.SeverityInfo:
			emoji = "ℹ️"
		}
		fmt.Fprintf(&b, "%s **%s** `%s:%d` — %s\n",
			emoji, f.Verdict, f.File, f.Line, strings.TrimSpace(f.Body))
	}
	return b.String()
}

// harnessRunner is the production Runner — wraps
// harness's runtime.Runtime. Defined in a separate file so
// this file stays focused on the orchestrator pipeline.
var _ Runner = (*harnessRunner)(nil)

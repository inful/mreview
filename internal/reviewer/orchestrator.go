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

	// DebugLLM, when true, prints the raw response from the
	// LLM (the text RunSync returns, before
	// ParseReviewResponse) to stderr with a clear separator.
	// Independent of --dry-run (which suppresses GitLab
	// side-effects) and --verbose (which raises the slog
	// level; DebugLLM writes raw bytes straight to stderr so
	// multi-line responses stay readable). False by default.
	DebugLLM bool

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
	// The check shells out to `git`. The distroless image does
	// NOT bundle a git binary, so the helper returns
	// ErrNoGit and we degrade silently to a debug-log; this
	// matches the build's "no extra binaries in the runtime
	// image" stance. Local dev (where git is on PATH) gets
	// the loud WARN that's actually useful.
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
			logger.Debug("skipping branch check: git not on PATH (CI / distroless)")
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
	userPrompt := prompts.ReviewUserPrompt(meta, chunks, *artifactSet)

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
//
// The Findings section is rendered as a Markdown table
// (severity emoji + verdict, file path, line, category, body)
// rather than a bullet list. The earlier bullet-list form
// (`%s **%s** \`%s:%d\` — %s\n`) was rendering as one
// squashed paragraph in GitLab because (a) adjacent bullets
// without a blank line collapse into a single line block and
// (b) long bodies wrap without internal breaks. The table
// gives each finding its own row with the body in its own
// cell — GitLab renders each row as a distinct line — and
// inline `<br>` separators keep multi-line bodies readable
// inside the cell.
//
// The whole table is wrapped in GitLab-Flavored Markdown's
// <details> collapsible block, with the finding count in the
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
func renderSummary(mr *gitlab.MergeRequest, summary string, findings []policy.EnforcedFinding) string {
	var b strings.Builder
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
	fmt.Fprintf(&b, "<details>\n<summary>Findings (%d)</summary>\n\n", len(findings))
	if len(findings) == 0 {
		b.WriteString("No issues found.\n")
	} else {
		b.WriteString("| Severity | File | Line | Category | Description |\n")
		b.WriteString("|----------|------|-----:|----------|-------------|\n")
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
			body := strings.TrimSpace(f.Body)
			// Markdown tables render newlines as a literal space
			// inside a cell; <br> is the documented GitLab-Flavored
			// Markdown escape for an in-cell line break.
			body = strings.ReplaceAll(body, "\r\n", "<br>")
			body = strings.ReplaceAll(body, "\n", "<br>")
			// Pipe would terminate the row; backslash-escape.
			body = strings.ReplaceAll(body, "|", `\|`)
			fmt.Fprintf(&b, "| %s %s | `%s` | %d | %s | %s |\n",
				emoji, f.Verdict, f.File, f.Line, f.Category, body)
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

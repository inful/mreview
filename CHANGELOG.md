# Changelog

All notable changes to mreview are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/), and this
project adheres to [Semantic Versioning](https://semver.org/).

Entries are generated from conventional commits by GoReleaser.

## [0.9.1]

### Fixed

- **Debug image is now usable as a GitLab CI runtime** (#44).
  The `*:debug` image variant shipped since v0.8.0 was
  technically a `distroless/cc:debug-nonroot` but its
  `/usr/bin/sh` was never installed in the runtime stage, so
  GitLab Runner's `script:` blocks failed with
  `exec: "/bin/sh": not found`. The debug image now `COPY`s
  the glibc-clean busybox from
  `distroless/cc:debug-nonroot`'s `/busybox/busybox` to
  `/usr/bin/sh`, and also places the mreview binary at
  `/usr/local/bin/mreview` so CI scripts can call
  `mreview review ...` without the absolute path.

## [0.9.0]

### Added

- **Bundled skills ship with every binary** (#44). Five
  review-guidance `.md` files are now embedded via
  `//go:embed`: `go-review`, `testing-patterns`,
  `error-handling`, `tokensave-usage`, `skill-authoring`.
  The `skill-authoring` skill is the meta-circular layout
  contract — the loader reads it on demand when reviewing
  MRs that touch a `skills/` directory.
- **Central skills repo support** (#44). Operators point
  `--skills-repo` at a GitLab project that hosts `.md`
  files; the loader merges the bundled set with the
  central repo and the central repo wins on name collision.
  Override semantics: drop `go-review.md` in your central
  repo to replace the bundled default.
- **`--debug-llm`** flag — prints the raw LLM response to
  stderr regardless of log level. Useful when the parser
  drops a finding and you want to see exactly what the
  model emitted.
- **`--max-output-tokens`** flag (default `16384`) — bound
  on each LLM generation request. Raise for verbose
  chain-of-thought models, lower for tight-budget models.
- **`--max-turns`** flag (default `6`) — cap on the agent's
  tool-use loop. Raise for complex MRs, lower for chatty
  models.
- **`--no-dedup`** flag — force a fresh review even when a
  prior bot summary with the same commit SHA already exists
  on the MR.
- **Required `--workdir`** flag — the harness previously
  read the agent's working directory from the operator's
  CWD, which silently indexed the wrong project and
  produced ENOENT-loop failures. The flag is now REQUIRED;
  `runReview` fast-fails with `ExitConfig` if unset.
  Tokensave is the only file-read path; the harness's
  local tool registry is empty.
- **Branch tracking for the MR source branch in
  tokensave.** Pre-tracks the branch before the harness
  spawns so the agent's first code-graph query is fast.
- **WARN on `--workdir` branch vs MR source branch
  mismatch.** Catches the operator mistake of pointing
  `--workdir` at a different branch than the MR's source.
- **Dedup by commit SHA + resolve prior findings.** If a
  prior mreview summary on the MR carries the current MR
  HEAD's commit SHA, the LLM run is skipped entirely (the
  prior review still represents this commit).
  `--no-dedup` forces a re-run.
- **Render the summary note as a Markdown table** (no
  squashed bullets), with the findings list wrapped in
  `<details><summary>` to keep the MR timeline clean.
- **`mcp__skills__list_skills` /
  `mcp__skills__read_skill`** — the skills MCP server
  exposes the loader's content over stdio to the harness.
  The harness spawns `mreview skills-mcp` as a subprocess
  when `--skills-repo` is set.
- **GitLab repository-files transport** — the skills
  loader reads `.md` files via `gitlab.Client`'s typed
  wrapper, sharing the retry / backoff / typed-error
  plumbing used by every other GitLab call.

### Refactored

- **Replace SHA-256 body fingerprint with `file:line`
  dedup.** The orchestrator now builds a `<path>:<line>`
  set from prior unresolved bot findings and suppresses
  new findings whose key matches. Wording drift between
  runs no longer causes near-duplicate posts. Resolved
  prior findings don't count (the operator marked them
  obsolete, so a re-run is a chance to spot something new
  at the same location). See
  [`internal/reviewer/file_line_dedup.go`](internal/reviewer/file_line_dedup.go).
- **Drop auto-resolve + content fingerprint.** Previously,
  a new commit auto-resolved any prior finding whose line
  had moved; that misled operators into thinking the
  issue was fixed. Resolving now happens only via the
  operator.

## [0.8.0]

### Added

- **Bundle tokensave in the mreview image** —
  `Dockerfile` and `Dockerfile.debug` now `COPY` the
  verified tokensave binary to
  `/usr/local/bin/tokensave`, pinned to v7.12.1 via build
  args (`TOKENSAVE_VERSION`, `TOKENSAVE_SHA_AMD64`,
  `TOKENSAVE_SHA_ARM64`). The CI smoke job
  (`scripts/tokensave-smoke.sh`) verifies the upstream
  `SHA256SUMS` matches before the image gets built.

### Fixed

- **Switch runtime base from `distroless/static` to
  `distroless/cc`.** Tokensave is dynamically linked
  against glibc + libgcc_s; the static base stripped the
  dynamic linker and the binary refused to exec with
  `not found` (the ELF interpreter). The `cc` variant
  includes glibc + libgcc at the cost of ~30 MB, which
  buys the tokensave binary plus its runtime libs.
- **Install `curl` in the Alpine tokensave build stage.**
  Busybox `wget` has no fail-on-error flag; the GitHub
  API download needs `-fsSL` semantics to follow
  redirects and fail on HTTP errors.
- **Spawn the tokensave MCP server with `serve --path`**
  instead of the older `mcp --project-root` shape. The
  v7.12.x CLI rejects the old flag layout. The smoke
  test (`scripts/tokensave-smoke.sh`) now blocks the
  build until the contract is corrected and is wired into
  `.github/workflows/ci.yml` as the `tokensave-smoke`
  job.

## [0.7.0] — architecture reset (#42)

This release is the breaking-change cutover from the
pre-reset architecture (webhook receiver + hand-rolled LLM
loop) to the post-reset architecture (CI-only + harness
library + tokensave MCP). Every operator upgrading needs
to re-read the README and rewire their CI.

### Removed

- **`mreview serve`** — the webhook-receiver subcommand
  is gone. CI is the canonical run mode; central CI
  definitions handle onboarding (see README → "Onboarding
  via central CI").
- **`internal/llm/`** (8 source files + tests) —
  replaced by the harness library's LLM loop.
- **`internal/server/`** (10 source files + tests) —
  webhook receiver, worker pool, queue, throttle. Dead
  with serve gone.
- **`cmd/mreview/serve.go`** — dropped from the CLI
  surface entirely.
- **`--llm-url`** — renamed to `--provider-base-url`.
- **`--llm-api-key`** — removed; harness reads
  provider-specific env vars (`ANTHROPIC_API_KEY`,
  `OPENAI_API_KEY`, `GOOGLE_API_KEY`, `LITELLM_API_KEY`,
  `OPENROUTER_API_KEY`).
- **`--temperature`, `--max-tokens`** — removed; the
  harness library owns these.
- **`--max-diff-bytes`, `--max-batch-bytes`,
  `--per-chunk-timeout`, `--chunk-retries`,
  `--allow-partial`** — removed; the harness owns
  chunking.
- **`examples/webhook-setup.md`,
  `examples/team-prompt-system.txt`,
  `examples/team-prompt-user.txt`** — referenced the
  dropped serve mode or pre-reset CLI flags.

### Added

- **Per-event review guard** (#41) — `internal/event/`.
  A pure function short-circuits `mreview review` with a
  clean exit 0 on events that would spam reviews
  (`merge_request_event` draft, `push`, `trigger`,
  `pipeline`, `parent_pipeline`, `webide`, `chat`,
  `ondemand_dast_*`, `security_orchestration_policy`,
  `external_pull_request_event`). Override via
  `--on-drafts=run` or `--on-push=run`.
- **`policy.yaml` enforcement + `ExitPolicy = 8`** —
  `internal/policy/`. The schema
  (`severity_overrides`, `forbid`, `require`, `labels`) is
  loaded and validated at startup; any `error`-verdict
  finding (or any `forbid` / `require` rule firing) makes
  the review exit with `8`, distinct from `7`
  (`ExitInternal`).
- **CI artifact reuse** (#43) —
  `internal/ci/artifact/`. The orchestrator reads
  `build.log`, `test_results.json`, `lint.json`, and
  `vulns.json` from `--artifacts-dir` (default
  `.mreview-artifacts`) and threads them into the harness
  prompt as pre-loaded context. Missing / empty /
  malformed artifacts are degraded information, not
  errors.
- **Tokensave as the primary MCP tool.** The harness
  agent routes all reads through `mcp__tokensave__*`
  (`--tokensave-enabled`, default `true`;
  `--tokensave-bin`, default PATH-resolved `tokensave`).
  When tokensave is unavailable, the agent falls back to
  read_file-only mode and the orchestrator logs a
  warning.
- **System prompt in
  `internal/prompts/review_system.md`** — embedded via
  `//go:embed` with golden-file tests pinning the
  read-only contract, JSON schema, severity levels,
  categories, CI artifact references, and policy
  awareness. Reviewers can now see prompt changes in
  plain markdown.
- **`examples/central-ci.yml`** — the reference template
  for the org-wide onboarding pattern. Producer stages
  (build / test / lint / vulns / tokensave-sync) +
  mreview stage in one pipeline.

### Changed

- **CLI is now `mreview review` + `mreview doctor` +
  `mreview skills-mcp`** (the latter hidden; spawned as
  a subprocess by the harness).
- **Provider matrix** is owned by the
  [harness](https://github.com/sausheong/harness)
  library. Six providers wired through `--provider`:
  `anthropic`, `openai`, `gemini`, `litellm`,
  `openrouter`, `local`.
- **`config.Defaults()`** sets `gitlab.url`,
  `provider.base_url`, `provider.model`,
  `review.bot_username_env`, and
  `retry.{max_attempts, initial_backoff, max_backoff}` so
  existing YAML configs keep working without explicit
  values.
- **Six LLM-call per-chunk + one merge call**, not one
  mega-call — the orchestrator chunks by file (with
  hunk-level splitting for huge files), each chunk fits
  the model's context, and the merge LLM call writes a
  single summary paragraph from all per-chunk reviews.

### Refactored

- **`internal/reviewer/` rewritten.** The hand-rolled
  chunk loop is gone; the orchestrator is a pure Go
  function (`internal/reviewer/orchestrator.go`) that
  calls into a `Runner` interface (`HarnessRunner` in
  production, `FakeRunner` in tests). The harness library
  owns the LLM loop, streaming, prompt caching, MCP
  connection lifecycle, and session persistence.
- **`cmd/mreview/main.go`'s `applyConfigToEnv`** is
  table-driven — the bindings are listed once and each
  entry's `get` function returns the value to set.
- **Read-only contract enforced by tests.** The
  harness's bash / edit_file / write_file tools are
  imported only by tests that prove they aren't
  registered; mreview never references them in
  production code.

## [0.6.0]

### Added

- **`--workers N`** flag on `mreview serve` for the
  worker-pool concurrency cap (#39). Steady-state
  concurrency (number of simultaneous review goroutines).
  Distinct from `queue_size` which is the burst buffer.
  Lower for slow / shared LLMs; raise for batched / fast
  inference.

### Refactored

- **Extract `buildClients` helper** shared by `review` and
  `serve` (#37). Both subcommands previously assembled the
  GitLab + LLM client triples independently; the helper
  eliminates the duplication.
- **Drop pointless wrappers, magic numbers, wrong
  comments, and a stale stub** across `cmd/mreview`,
  `internal/llm`, and `internal/server` (#38). Cleanup in
  service of the upcoming architecture reset (#42).

## [0.5.1]

### Fixed

- **Wire the upstream `Retry-After` header into the retry
  backoff** (#36). The retry path's `retryAfterDuration`
  only read `e.Body` — every 503 / 429 with a `Retry-After`
  hint effectively went unheeded. `(*Client).classify` now
  captures the upstream header on `*Error.RetryAfter`;
  `retryAfterDuration` prefers the header (via the
  canonical `parseRetryAfterHeader`, handling
  delta-seconds and HTTP-date forms) and falls back to
  the body for old hand-constructed errors.
- **Drop the unused `config.MustEnv` helper.** It was
  exported, tested in the same package, and never called
  by any production code. The package is `internal/`, so
  no external embedder can be depending on it.

## [0.5.0]

### Added

- **Chunk-level retry mechanism with `--chunk-retries` flag** (#17).
  When a chunk's LLM call fails with a transient error (currently:
  per-call timeout), the reviewer retries the same chunk up to N
  times before giving up. Exponential backoff: 500 ms, 1 s, 2 s,
  … capped at 5 s. Default 1 (one retry, two total attempts per
  chunk). The openai-go client's own `MaxRetries=2` still catches
  5xx / 429 at the transport layer; this layer catches the timeout
  case that bubbles past it. Each retry attempt logs at Debug; the
  final exhaustion logs at Warn. Env: `MREVIEW_CHUNK_RETRIES`.
  YAML: `review.chunk_retries`.
- **`--allow-partial` escape hatch for atomic-failure** (#31). When
  false (the default), any chunk failure aborts the review with a
  `*ChunkFailureError` before posting anything to GitLab. When
  true, the legacy "log a warn and continue with empty findings"
  path is restored. Use this on big MRs where one bad chunk isn't
  worth aborting the whole run. Env: `MREVIEW_ALLOW_PARTIAL`.
  YAML: `review.allow_partial`.

### Changed

- **Reviews now fail atomically when a chunk can't be reviewed**
  (#31). Previously, when a chunk's LLM call failed after retries,
  the reviewer substituted an empty result and continued with the
  chunks that did succeed. The merge step's "fallback concatenation"
  path silently filled in — the summary note still got posted even
  though the operator couldn't tell the difference between a bot
  that said "LGTM" on a partial review and one that said "LGTM" on
  a full review. After this release, the default behaviour is
  atomic failure: the whole review aborts before anything posts,
  the CLI exits 7, and the operator re-runs. The error message
  carries the batch index, file list, attempt count, and underlying
  error so the failure is actionable. Use `--allow-partial=true`
  for the previous behaviour.

### Refactored

- **Split `internal/reviewer/reviewer.go`** (#5) from 917 LOC into
  eight focused files (`config.go`, `result.go`, `filter.go`,
  `batch.go`, `format.go`, plus extensions to `dedupe.go` /
  `post_summary.go` / `summary.go`). The orchestrator itself dropped
  to 389 LOC. No behaviour change.
- **Collapsed six duplicated `classify*Error` wrappers** (#3) in
  `internal/gitlab` into a single
  `(*Client).classify(method, url, resp, err, extraClassifiers...)`
  method. The line-range special case in `PostDiscussion` is now
  passed as an inline classifier closure. The body-extraction
  logic (priority: upstream `ErrorResponse.Body` → `Message` → live
  `resp.Body`) is shared by every endpoint, not just
  `PostDiscussion`. No behaviour change.
- **Extracted a single shared `truncate` helper** (#4) into a new
  `internal/strutil` package as `Truncate(s, maxBytes)`, unified on
  the `"..."` suffix. Five copies (across `cmd/mreview`,
  `internal/reviewer`, `internal/llm`, `internal/gitlab`) collapsed
  into one. No behaviour change beyond the suffix string (`"..."`
  vs the old `"...(truncated)"` in two call sites; the `internal/gitlab`
  classification test was updated to match).

## [0.4.1]

### Added

- **Debug-level LLM logs under `--verbose`** (#26). Operators running
  with `--verbose` were seeing no extra output during successful
  review runs because no Debug-level logs fired between the LLM call
  and the result — the essential diagnostic for "the model produced
  `findings: []` but the summary mentions real issues" was missing.
  mreview now logs `llm prompt`, `llm raw response`, `llm merge
  prompt`, and `llm merge raw response` at Debug level so operators
  can confirm what the model actually sees and produces without
  reaching for external tooling.

### Fixed

- **Findings are now reliably recovered through the LLM review
  pipeline** (#25). Three distinct failure modes were dropping or
  fabricating findings between the chunk LLMs and the final MR
  post: capable local models emitted `findings: []` alongside
  multi-paragraph summaries listing real bugs (the model had the
  data, it just chose not to put it in the array); the merge LLM
  was renaming `body` to `message` so every merged finding was
  silently dropped by the empty-body filter; and the merge LLM was
  hallucinating new claims (e.g. "MR contains no Go source code")
  because the chunk LLMs' empty finding slices gave it no signal
  about file scope. The pipeline now requires findings as the
  primary output (summary is a recap, not a substitute), preserves
  the Finding schema inline in the merge prompt with an explicit
  anti-rename guard, forbids the merge LLM from synthesising new
  findings ("reducer, not a generator"), and threads prior-batch
  findings into the next chunk's prompt so later chunks (and the
  merge step) have ground truth about earlier batches' scope.

- **Truncated LLM responses no longer lose every finding** (#25).
  When the LLM hit `MaxTokens` or the stream was cut mid-finding,
  all three prior parsing strategies failed because no closing
  delimiter ever arrived — raw `Unmarshal` hit `ErrUnexpectedEOF`,
  no fence was present, and the loose brace-matcher never balanced.
  A fourth strategy uses `json.Decoder` to walk the response and
  recover whichever findings completed before the cut; partial
  findings at the truncation point are silently dropped. The
  final parse-failure error now distinguishes "response appears
  truncated — raise `MaxTokens` or batch size" from "response is
  structurally unbalanced but does not look truncated — likely
  invalid JSON from LLM", so operators reading parse-failure logs
  get an actionable diagnostic at a glance.

- **`golangci-lint v2 schema compliance** in `.golangci.yml`. The
  config had a top-level `exclusions:` block that
  `golangci-lint v2.13.2`'s strict schema rejects
  (`additional properties 'exclusions' not allowed`), which had
  been blocking the `Lint & test` CI check on every push since
  2026-09-22. Moved to `linters.exclusions` and dropped the v1
  `default:` field, leaving an empty rules list with no
  behavioural change for this repo.

## [0.4.0]

### Added

- **`max_batch_bytes` field on `LLMPreset`** (#24). Operators whose
  models have an effective context smaller than their advertised
  window (common for heavily quantized local models) can now tune
  the per-call packing budget directly in YAML instead of relying
  on the derivation from `context_window`. A model that returns
  empty content on a packed batch (rather than timing out) — the
  classic "prompt technically fits but the model gives up" pattern
  — can now be configured with a smaller preset value. Three-layer
  precedence: CLI flag > preset field > derived from
  `context_window`. The shipped `coding-agent` preset now carries
  the new field.

## [0.3.1]

### Fixed

- **Inline discussion posts on the right side of the diff.** mreview
  was sending `old_path: ""` and `old_line: 0` unconditionally, which
  GitLab's `/discussions` endpoint rejects with a generic 500 (the
  diff-line lookup fails when asked to anchor to old line 0). The
  summary endpoint doesn't take a position so it kept posting fine
  while every inline comment was lost. The reviewer now constructs
  the position per file status: new files send `new_path +
  new_line`, modified files do the same (with `old_path` only when
  renamed), and deleted files send `old_path + old_line`. Fields
  that don't apply are omitted from the request body entirely
  (nil pointers + `omitempty`), so GitLab sees exactly the shape
  the API docs describe. (#23)

## [0.3.0]

### Added

- **`reasoning_effort` parameter for o-series models** (#21). New
  `--reasoning-effort` flag (`low` / `medium` / `high`) and a
  matching `reasoning_effort` field on `llm_presets.<name>` so
  reasoning-capable models (OpenAI o1/o3, Azure AI Foundry, Groq,
  Together, etc.) take the budget the operator specifies. Local
  non-reasoning models and non-supporting providers ignore the
  parameter on the wire; existing users see no change.

## [0.2.0]

### Fixed

- **Reviews now post inline comments instead of silently dropping them** (#15).
  The previous system prompt instructed the LLM to emit an empty
  `findings` array on "clean" diffs, which a 7B coder model took as
  permission to skip findings entirely — the resulting MR had a
  summary note but no inline comments even when the LLM had clearly
  identified real issues in the summary prose. The prompt now
  requires at least one finding per substantive diff; severity
  `info` is the acceptable escape hatch for genuinely clean diffs.
  Pair with a WARN log when zero findings come back on a diff
  larger than 10 lines, so the regression is impossible to miss.
- **Dropped chunks are now identifiable in logs** (#16). The
  `chunk review failed` WARN line previously carried only the error
  message — no batch index, no file paths. Now it carries
  `batch=<n> files=<csv>` so operators can identify which file was
  lost in a multi-chunk MR without re-reading every prompt.

### Added

- **Chunk packing for large-context models** (#18). New
  `--max-batch-bytes` flag and `BatchChunksWithLimit` packer
  greedily group consecutive chunks whose combined size fits the
  budget. A 30-file MR against an Opus-tier 168k-context model
  collapses from 30 LLM calls to 1-3. Default is `0` (one call per
  chunk) so existing users see no change.
- **LLM presets in YAML config** (#20). New `llm_presets:` and
  `llm_preset_by_model:` blocks let operators declare each model's
  `context_window` once and have mreview auto-derive the packing
  budget (`max_batch_bytes = (context_window - 1500 - max_tokens
  - 15% safety) × 4`). CLI flags always override preset values;
  every decision is logged.

### Refactored

- **Split `internal/gitlab/comments.go`** (#13) into four files
  by responsibility: `types.go` (Note, Discussion, InlineComment +
  projections), `post_summary.go`, `post_discussion.go`,
  `validate.go`. Largest non-test file in the package dropped from
  369 LOC to 191 LOC. No behaviour change.

## [0.1.0] — initial release

### Added

- **`mreview review`** — review one merge request end-to-end:
  fetch → chunk → call LLM → parse → dedupe → post summary +
  inline comments.
- **`mreview serve`** — webhook server that consumes GitLab
  `merge_request` events, validates via `X-Gitlab-Token` HMAC,
  and dispatches reviews through a bounded worker pool with
  graceful shutdown.
- **`mreview doctor`** — health check for GitLab token + LLM
  endpoint + configured model presence.
- **LLM provider** (`internal/llm`): OpenAI-compatible
  (`/v1/chat/completions`), covering Ollama, llama.cpp, vLLM, and
  LM Studio via base URL swap. Auto-appends `/v1` when missing.
- **Resilient LLM JSON parser**: raw → fenced → loose bracket
  extraction, with bare-array recovery and `hasReviewKeys`
  validator (no false-positive matches on stray `{...}` in code
  snippets).
- **Per-file diff chunker** with hunk-level splitting; surfaces
  `OversizedError` for files too big to chunk even after split.
- **Fingerprint-based dedupe** (SHA-256 of normalized body) so
  re-running the review is idempotent. Bot-author filter via
  `GITLAB_BOT_USERNAME` to avoid stepping on human comments.
- **LLM-call hallucination guard**: findings whose file path
  isn't in the diff are dropped before posting.
- **Per-finding line-out-of-range tolerance**: GitLab 400s with
  the "line out of range" signal are classified as `KindConflict`
  and the single finding is dropped without failing the review.
- **Multi-arch static binary** (linux/darwin/windows × amd64/arm64,
  CGO disabled, distroless container images).
- **GitHub Actions CI** (lint + race-enabled test + build matrix
  on 6 platform combos) and **release workflow**
  (GoReleaser → GitHub Release + GHCR).

### Subcommand flags (highlights)

- `--gitlab-token` / `GITLAB_TOKEN` — Personal Access Token with
  `api` scope (required).
- `--llm-url` / `LLM_URL` — defaults to `http://localhost:11434/v1`.
- `--model` / `LLM_MODEL` — defaults to `qwen2.5-coder:7b`.
- `--max-diff-bytes` / `MREVIEW_MAX_DIFF_BYTES` — per-chunk byte
  budget (default 200 KB).
- `--per-chunk-timeout` / `MREVIEW_PER_CHUNK_TIMEOUT` — per-LLM-call
  timeout (default 120 s).
- `--retries` / `MREVIEW_RETRIES` — GitLab retry count on
  transient 5xx / 429 (default 3).
- `--retry-backoff` / `MREVIEW_RETRY_BACKOFF` — initial backoff;
  exponential with jitter.
- `--ignore-paths` / `MREVIEW_IGNORE_PATHS` — doublestar glob
  patterns to skip files (e.g. `**/*.pb.go`, `vendor/**`).
- `--comment-mode` / `MREVIEW_COMMENT_MODE` — choose between
  `both` (default), `inline-only`, or `summary-only`.
- `--required-label` / `MREVIEW_REQUIRED_LABEL` (serve only) —
  gate reviews to MRs carrying a specific label.
- `--bot-username` / `GITLAB_BOT_USERNAME` — username of the
  posting token; used by dedupe to scope to the bot's own comments.
- `--throttle-window` / `MREVIEW_THROTTLE_WINDOW` (serve only) —
  per-MR webhook cooldown (default 30 s; 0 disables).
- `--config` / `MREVIEW_CONFIG` — path to a YAML config file
  (`~/.config/mreview/config.yaml` by default).
- `--system-prompt-file` / `MREVIEW_SYSTEM_PROMPT_FILE` — file
  with team-specific text appended to the system prompt (after
  the system-owned schema).
- `--user-prompt-file` / `MREVIEW_USER_PROMPT_FILE` — file with
  per-MR context appended to the user prompt.

### Lifecycle handling

- **Open / Reopen events**: full review runs; summary note posted.
- **Update events** (new commits): full review runs; only inline
  findings posted (summary is skipped to avoid accumulating
  stale summary notes on every push — the inline findings carry
  the per-push signal).
- **Close / merge / approve**: silently ignored (HTTP 204).
- **Per-MR webhook throttle** (default 30 s): rapid-fire duplicate
  deliveries for the same MR are acknowledged with HTTP 202 but
  not queued — the in-flight / just-finished review covers the
  latest commit anyway.
- **Bounded memory**: throttle map self-evicts at 10 000 entries
  (FIFO); background sweep drops stale entries every
  `window/2`.

### Operator ergonomics

- **`mreview doctor`** for pre-deploy health checks with
  `--skip-gitlab` / `--skip-llm` flags for partial diagnostics.
- **`--dry-run`** on `mreview review` logs every LLM call +
  GitLab post without performing any of them.
- **Structured logging** (JSON or text) via `--log-format` /
  `MREVIEW_LOG_FORMAT`.
- **Prompts are layer-customizable**: the system-owned schema +
  output rules are immutable; teams append guidance via
  `--system-prompt-file` (always-on conventions) and
  `--user-prompt-file` (per-MR context). Default prompts are
  unchanged when these flags are unset.

### Documentation

- `README.md` (operator-facing, ~790 lines): why / how /
  install / quickstart / all three subcommands / example output /
  exit codes / webhook setup / configuration / prompt
  customization / architecture narrative / performance & cost /
  examples index / development / troubleshooting / license.
- `CONTRIBUTING.md`: TDD workflow, conventional commits,
  extension points for adding new LLM providers and GitLab
  client methods.
- `CHANGELOG.md`: this file.
- `SECURITY.md`: GitHub Security Advisories-based disclosure
  policy with response SLA.
- `examples/`: copy-pasteable starting points — `config.yaml`,
  `docker-compose.yml`, `gitlab-ci.yml`, `webhook-setup.md`,
  `team-prompt-system.txt`, `team-prompt-user.txt`.

### License

GNU Affero General Public License v3.0 or later (AGPL-3.0-or-later).
The AGPL network clause applies: anyone running a modified
mreview as a service that others interact with over a network
must provide the source of their modifications to those users.


[Unreleased]: https://github.com/inful/mreview/compare/v0.9.1...HEAD
[0.9.1]: https://github.com/inful/mreview/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/inful/mreview/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/inful/mreview/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/inful/mreview/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/inful/mreview/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/inful/mreview/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/inful/mreview/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/inful/mreview/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/inful/mreview/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/inful/mreview/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/inful/mreview/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/inful/mreview/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/inful/mreview/releases/tag/v0.1.0
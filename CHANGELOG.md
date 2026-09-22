# Changelog

All notable changes to mreview are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/), and this
project adheres to [Semantic Versioning](https://semver.org/).

After v0.1.0, entries are generated from conventional commits by
GoReleaser. The hand-written entries below document the initial
release.

## [Unreleased]

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

[Unreleased]: https://github.com/inful/mreview/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/inful/mreview/releases/tag/v0.1.0

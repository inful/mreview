# mreview

Automated LLM merge-request reviewer for GitLab.

`mreview` fetches the diff for a merge request, sends it to a local LLM
(Ollama, llama.cpp, vLLM, LM Studio — any OpenAI-compatible endpoint), and
posts the result back to the MR as inline line comments plus a summary
thread.

Single static Go binary. No daemons, no cloud dependency, no data leaves
your network.

```text
$ ./mreview review --repo=group/project --mr=42 --dry-run --log-format=json
{"time":"...","level":"INFO","msg":"starting review","repo":"group/project","mr":42,...}
{"time":"...","level":"INFO","msg":"fetched MR","title":"Add caching layer","state":"opened"}
{"time":"...","level":"INFO","msg":"chunked diff","chunks":3}
{"time":"...","level":"INFO","msg":"reviewing chunk batch","batch":1,"chunks":1}
{"time":"...","level":"INFO","msg":"review complete","findings_total":2,"findings_posted":2,"summary_posted":true}
https://gitlab.example.com/group/project/-/merge_requests/42
```

## Table of contents

- [Why mreview](#why-mreview)
- [How it works](#how-it-works)
- [Install](#install)
- [Quickstart](#quickstart)
- [Subcommands](#subcommands)
- [What the bot posts](#what-the-bot-posts)
- [Exit codes](#exit-codes)
- [GitLab webhook setup (for `serve`)](#gitlab-webhook-setup-for-serve)
- [Configuration](#configuration)
- [Customizing the prompts](#customizing-the-prompts)
- [LLM presets](#llm-presets)
- [Architecture](#architecture)
- [Performance & cost](#performance--cost)
- [Examples](#examples)
- [Development](#development)
- [Troubleshooting](#troubleshooting)
- [Contributing](CONTRIBUTING.md)
- [Security](SECURITY.md)
- [Changelog](CHANGELOG.md)
- [License](#license)

## Why mreview

- **Local-first.** No data leaves your machine — the LLM runs on your
  hardware, the GitLab token never reaches a third party.
- **Idempotent.** Re-running on the same MR posts only new findings; the
  fingerprint-based dedupe layer (SHA-256 of normalized body + author
  filter) skips anything the bot already commented on.
- **Resilient.** JSON from local LLMs is messy; the parser handles raw
  output, fenced blocks, bare arrays, and prose-wrapped JSON. Per-finding
  line-out-of-range errors are skipped without failing the review.
- **Multi-LLM.** Same binary talks to Ollama, llama.cpp, vLLM, or LM
  Studio via the OpenAI-compatible `/v1` endpoint — no provider SDK.
- **Single static binary.** CGO disabled, distroless base image, ~20 MB
  compressed; runs anywhere.

## How it works

```
┌──────────────┐  ┌────────────────────┐  ┌──────────────────┐
│ mreview serve │──│  HTTP /webhook      │──│  Worker pool      │
│              │  │  verify X-Gitlab-    │  │  (4 goroutines,   │
│ GitLab       │  │  Token HMAC          │  │  bounded queue)   │
│ sends MR     │  └────────────────────┘  └────────┬─────────┘
│ events here  │                                     │
└──────────────┘                                     │
                                                    ▼
                          ┌──────────────────────────────────┐
                          │  Reviewer.ReviewMR              │
                          │                                  │
                          │  1. FetchMR / FetchChanges       │
                          │  2. ChunkByFile                  │
                          │  3. Per chunk: BuildReviewPrompt │
                          │     + LLM Chat                   │
                          │     + ParseReviewResponse       │
                          │  4. Consolidate (multi-chunk)    │
                          │  5. FilterFindings (hallucinated  │
                          │     paths, empty bodies)         │
                          │  6. ListDiscussions → fingerprint│
                          │     dedupe set                   │
                          │  7. PostSummary + PostDiscussion │
                          └──────────┬───────────────────────┘
                                     │
                                     ▼
                          ┌──────────────────────────────────┐
                          │  Local LLM (Ollama / llama.cpp /  │
                          │  vLLM / LM Studio)                │
                          │  OpenAI-compatible /v1 endpoint   │
                          └──────────────────────────────────┘
```

`mreview review` is the same pipeline but skips the webhook front-end;
it's the right entrypoint for CI jobs, cron, and `make review-MR123`.

## Install

### Pre-built binary

```bash
# Latest release:
curl -L -o mreview.tar.gz \
  https://github.com/inful/mreview/releases/latest/download/mreview_Linux_x86_64.tar.gz
tar -xzOf mreview.tar.gz mreview > /usr/local/bin/mreview
chmod +x /usr/local/bin/mreview

# Pin to a version:
curl -L -o mreview.tar.gz \
  https://github.com/inful/mreview/releases/latest/download/mreview_0.1.0_Linux_x86_64.tar.gz
```

### Container image

```bash
docker pull ghcr.io/inful/mreview:latest
docker run --rm ghcr.io/inful/mreview:latest --help

# Debug image (with busybox shell for `docker exec` debugging):
docker pull ghcr.io/inful/mreview:latest-debug
```

### `go install`

```bash
go install github.com/inful/mreview/cmd/mreview@latest
```

## Quickstart

```bash
# 1. Verify your wiring before posting anything to GitLab.
mreview doctor --skip-llm   # GitLab check (needs $GITLAB_TOKEN)
mreview doctor --skip-gitlab # LLM check (needs running Ollama / etc.)

# 2. Dry-run a real review. Logs every LLM call + GitLab post it
#    WOULD make, without actually posting. Cheap, safe, fast.
GITLAB_TOKEN=glpat-xxx \
mreview review \
    --repo=group/project \
    --mr=42 \
    --dry-run \
    --log-format=json

# 3. Same command, no --dry-run. Comments land on the MR.
GITLAB_TOKEN=glpat-xxx \
mreview review --repo=group/project --mr=42

# 4. Long-running webhook consumer.
GITLAB_TOKEN=glpat-xxx \
GITLAB_WEBHOOK_SECRET=mysecret \
mreview serve --addr=:8080

# 5. Or persist the connection settings in a config file so
#    every invocation doesn't repeat them.
mkdir -p ~/.config/mreview
cp examples/config.yaml ~/.config/mreview/config.yaml
$EDITOR ~/.config/mreview/config.yaml    # set gitlab.url + llm.base_url
GITLAB_TOKEN=glpat-xxx \
mreview review --repo=group/project --mr=42   # flags win over config
```

## Subcommands

### `mreview review`

Review one merge request and exit.

```text
Usage: mreview review --repo=STRING --mr=INT --gitlab-token=STRING [flags]

Flags:
      --repo=STRING                              (required) Repository path (group/project).
      --mr=INT                                   (required) Merge request IID.
      --gitlab-url="https://gitlab.com"           GitLab base URL ($GITLAB_URL).
      --gitlab-token=STRING                      (required) Personal Access Token ($GITLAB_TOKEN).
      --llm-url="http://localhost:11434/v1"        LLM OpenAI-compatible base URL ($LLM_URL).
      --llm-api-key=STRING                        LLM API key ($LLM_API_KEY).
      --model="qwen2.5-coder:7b"                  LLM model name ($LLM_MODEL).
      --temperature=0.2                           LLM sampling temperature.
      --max-tokens=2048                           LLM max output tokens per call.
      --reasoning-effort=STRING                    Reasoning budget for o-series-style models:
                                                  'low' / 'medium' / 'high'. Empty = server default.
                                                  No-op for models that don't support the field.
      --max-diff-bytes=200000                     Per-chunk byte budget.
      --max-batch-bytes=INT                       Byte budget for packing multiple chunks into one
                                                  LLM call. 0 (default) = one chunk per call.
      --per-chunk-timeout=2m0s                    Per-LLM-call timeout.
      --bot-username=STRING                       Bot username for dedupe ($GITLAB_BOT_USERNAME).
      --retries=3                                 GitLab API retry attempts on transient errors.
      --retry-backoff=500ms                       Initial retry backoff; exponential with jitter.
      --dry-run                                   Log intended LLM and GitLab calls without performing them.
```

The GitLab token must have `api` scope. The LLM URL must be an
OpenAI-compatible `/v1` endpoint (the provider appends `/v1` automatically
when missing — covers Ollama without forcing the user to type it).

### `mreview serve`

Run a webhook server that consumes GitLab `merge_request` events.

```text
Usage: mreview serve [flags]

Flags:
      --addr=":8080"                              HTTP listen address.
      --webhook-secret=STRING                     GitLab webhook shared secret ($GITLAB_WEBHOOK_SECRET).
      --queue-size=32                             Max concurrent reviews queued.
      --shutdown-timeout=30s                      Graceful shutdown drain timeout.
      --gitlab-url=...                            (same as review)
      --gitlab-token=STRING                       (required, $GITLAB_TOKEN)
      --llm-url=..., --llm-api-key=STRING, --model=STRING, ...
                                                 (same as review)
```

`serve` registers `POST /webhook` (the GitLab webhook target) and
`GET /healthz` (queue depth + capacity in JSON). It exits 0 when
shutdown completes cleanly.

### `mreview doctor`

Validate config + LLM + GitLab connectivity. Useful in CI / pre-deploy.

```text
Usage: mreview doctor [flags]

Flags:
      --gitlab-url=...                            (same as review)
      --gitlab-token=STRING                       ($GITLAB_TOKEN)
      --llm-url=..., --llm-api-key=STRING, --model=STRING, ...
                                                 (same as review)
      --max-diff-bytes=200000                     (matches review)
      --per-chunk-timeout=2m0s                    (matches review)
      --skip-gitlab                               Skip the GitLab check.
      --skip-llm                                  Skip the LLM check.
```

Example output:

```text
mreview doctor
─────────────
GitLab: ok  alice
  URL: https://gitlab.example.com
  Name: Alice Example

LLM: ok  qwen2.5-coder:7b available (of 3)
  URL: http://localhost:11434/v1
  Model present
  Models: qwen2.5-coder:7b, llama3:8b, mistral:7b

Config: ok  --max-diff-bytes=200000  --per-chunk-timeout=2m0s  model=qwen2.5-coder:7b

All checks passed.
```

On failure (e.g. wrong token, unreachable LLM) the subcommand exits 1
with a per-check breakdown.

## What the bot posts

A successful review produces **one summary note** + **zero or more
inline discussion threads** anchored to specific lines. The
shapes below come from `internal/reviewer/summary.go` and the
GitLab discussion API; you'll see them in the MR timeline as
soon as the first review completes.

### Summary note

A single top-level note (not anchored to any line). Posted once
per MR on `open` / `reopen` events; **skipped on `update`** so
repeated pushes don't pile up stale summaries (see [Performance &
cost](#performance--cost)).

```markdown
## mreview summary

LGTM with 2 nitpicks — secret caching and missing test for the
new eviction hook.

---
<details>
<summary>Findings (2)</summary>

| Severity | File:Line | Category | Comment |
|---|---|---|---|
| warning | `internal/auth/jwt.go:142` | security | JWT secret should be cached |
| info | `internal/cache/cache.go:55` | test | Missing test for new eviction hook |

</details>
```

The `<details>` block keeps the MR timeline clean — the table is
collapsed by default; readers expand it to see the per-finding
breakdown.

### Inline discussion

One per finding, anchored to `file:line` at HEAD using the MR's
`diff_refs`. The bot picks `NewLine` for additions/modifications
and `OldLine` for deletions (the GitLab discussion API requires
this for the position to render correctly).

```
**[warning]** jwt secret should be cached

The JWT signing secret is loaded from the environment on every
request. Cache it in a package-level var so we don't hit the env
lookup per call.

```suggestion
var jwtSecret = mustGetenv("JWT_SECRET")
```
```

The triple-backtick block tagged `suggestion` is GitLab's
"one-click apply" — the human reviewer can commit the suggested
change directly from the comment. The empty lines at the start and
end of the fence are intentional; GitLab's suggestion parser
needs them to strip the leading whitespace.

### When the bot stays silent

The bot posts **nothing** when:

- The diff is empty (trivial MR, e.g. comment-only changes) — the
  filter still runs but no chunks survive.
- Every finding is deduped against the bot's prior comments — no
  new comments to post. The summary also stays silent in this case
  (see the `update`-skip behavior).
- Every chunk's LLM call fails (transient errors exhaust the
  retry budget). The worker logs the failure; nothing posts.

Operators running with `--dry-run` see every post *attempted* in
the logs (with body and URL) without anything actually landing on
GitLab — use this to verify what the bot will say before letting it
hit a real MR.

## Exit codes

| Code | Meaning                                          |
|------|--------------------------------------------------|
| `0`  | Success                                          |
| `2`  | Configuration error (missing flag, bad flag, missing config) |
| `3`  | Authentication error (GitLab or LLM rejected)    |
| `4`  | Not found (MR, project, model)                   |
| `5`  | Conflict (line anchor out of range, stale MR head) |
| `6`  | Transient failure exhausted (5xx / 429 retries)  |
| `7`  | Unexpected internal error                        |

CI scripts can branch on these. `mreview review` and `mreview serve`
follow the same table; `mreview doctor` uses 0 / 1 (it always runs to
completion so it can report failures).

## GitLab webhook setup (for `serve`)

1. Generate a webhook secret (any random string; e.g. `openssl rand -hex 32`).
   Pass it to `mreview serve` as `--webhook-secret` or
   `GITLAB_WEBHOOK_SECRET`.
2. In your GitLab project: **Settings → Webhooks**.
3. **URL**: `http://<your-host>:8080/webhook` (or whatever `--addr` you
   picked).
4. **Secret token**: the same secret as in step 1.
5. **Trigger**: ✅ Merge request events. Leave others off so mreview only
   fires on the events it cares about.
6. **SSL verification**: enable when the URL is HTTPS; disable only for
   local development.
7. Save.

Test the webhook in the GitLab UI ("Test → Push events") to confirm the
URL is reachable. Push events return HTTP 204 (mreview ignores non-MR
events but acknowledges the delivery).

## Configuration

Three layers, in priority order (CLI flags always win):

1. **CLI flags** — e.g. `--gitlab-url=https://example.com`
2. **Environment variables** — `GITLAB_URL`, `LLM_MODEL`, etc. (every
   flag has a matching `env:` tag; see `--help` for each subcommand)
3. **YAML config file** at `--config` / `$MREVIEW_CONFIG` /
   `~/.config/mreview/config.yaml`. Values from the config populate
   env vars BEFORE flag parsing, so any flag the user doesn't set
   picks up the config value.

The config schema (see [`examples/config.yaml`](examples/config.yaml))
mirrors every flag. Secret fields reference env-var NAMES — never
store the secret itself in the file.

```yaml
# ~/.config/mreview/config.yaml
gitlab:
  url: https://gitlab.example.com
  token_env: GITLAB_TOKEN              # the token lives here, in the env
llm:
  base_url: http://localhost:11434/v1
  model: qwen2.5-coder:7b
review:
  max_diff_bytes: 200000
  temperature: 0.2
server:
  addr: ":8080"
  webhook_secret_env: GITLAB_WEBHOOK_SECRET
  queue_size: 32
```

### LLM presets

When you run mreview against multiple LLM backends with different
context windows, declaring each model's `context_window` once lets
mreview auto-derive the right packing budget per invocation. The
operator writes the YAML once; the `--model` flag selects the preset.

```yaml
llm_presets:
  opus-local:
    context_window: 168000
    per_chunk_timeout: 15m
  gemma-26b-e4b:
    context_window: 80000
    per_chunk_timeout: 5m
  o-series:
    context_window: 200000
    per_chunk_timeout: 15m
    reasoning_effort: high
  coding-agent:
    # Model's effective context is smaller than advertised; the
    # max_batch_bytes override pins batch size to a value the model
    # can process within per_chunk_timeout. Tune by trying
    # --max-batch-bytes=N at the CLI and seeing what completes.
    context_window: 168000
    per_chunk_timeout: 5m
    max_batch_bytes: 3000

llm_preset_by_model:
  "qwen2.5-coder:7b": opus-local
  "gemma-26b-e4b":    gemma-26b-e4b
  "o3-mini":          o-series
  "coding-agent":     coding-agent
```

When `--model=qwen2.5-coder:7b` matches, mreview derives
`--max-batch-bytes ≈ 520 KB` from the 168k window and uses the
15-minute timeout — no per-invocation flag wrangling. Lookup is
strict (exact match); unknown models get no preset and packing
falls back to disabled (one LLM call per file).

The byte budget is derived as:

```
max_batch_bytes = (context_window - 1500 - max_tokens - 15% safety) × 4 bytes/token
```

CLI flags (`--max-batch-bytes`, `--per-chunk-timeout`,
`--reasoning-effort`) always win when set; the preset fills in the
gaps. When a preset applies, you'll see log lines like:

```
INFO LLM preset applied                 preset=opus-local model=qwen2.5-coder:7b context_window=168000
INFO max_batch_bytes derived from preset preset=opus-local context_window=168000 max_tokens=8192 max_batch_bytes=532432
INFO max_batch_bytes from preset         preset=coding-agent max_batch_bytes=3000 derived_value=532432
INFO reasoning_effort from preset        preset=o-series reasoning_effort=high
```

Three sources of `max_batch_bytes`, in priority order:

1. **CLI flag** `--max-batch-bytes=N` (one-off override)
2. **Preset field** `max_batch_bytes: N` (per-model tuning)
3. **Derived from `context_window`** (sensible default)

Use the preset field when the model's effective context is smaller than its advertised window — common for heavily quantized local models that return empty content (rather than timing out) on prompts technically within the byte budget.

The optional `reasoning_effort` field on a preset (or the matching
`--reasoning-effort` CLI flag) controls the reasoning budget for
o-series-style models — OpenAI's o1/o3, Azure AI Foundry, Groq,
Together, and any other provider that proxies them. Allowed
values are `low`, `medium`, `high`. Local non-reasoning models and
non-supporting providers ignore the field on the wire.

## Customizing the prompts

The system prompt that goes to the LLM is **layered**:

1. **System-owned prefix** (immutable) — JSON output schema, severity
   semantics, the "be terse, output directly, don't wrap in fences"
   rules. Operators cannot override this. If they try (by, say,
   asking for a different output format), the parser still expects
   the standard ReviewResponse schema and parsing succeeds.

2. **Operator-supplied suffix** (optional) — appended after the
   system-owned rules. Use this for team conventions, language
   preferences, documentation standards, severity semantics your
   team uses, etc. Sits inside a labeled
   `# Team-specific guidance (operator-supplied)` section so the
   LLM can tell where it begins.

Similarly, the user prompt is layered:

1. **System-owned** — MR header (IID, title, author, branches),
   description (when `IncludeDescription` is on), per-file chunks
   with the `=== File: ... ===` envelope.
2. **Operator-supplied suffix** (optional) — appended after the
   chunks. Use this for per-MR context: "this MR is a WIP",
   "this is a dependency bump", "this touches a hot path".

Provide the two via flags (or matching env vars):

```bash
mreview review \
    --repo=group/project --mr=42 \
    --system-prompt-file=/etc/mreview/team-rules.md \
    --user-prompt-file=/etc/mreview/current-mr-context.md
```

See [`examples/team-prompt-system.txt`](examples/team-prompt-system.txt)
and [`examples/team-prompt-user.txt`](examples/team-prompt-user.txt)
for worked examples. The defaults (no flag) are the system-owned
prompts unchanged — your text is purely additive.

**What you CAN'T do with these files:** redefine the JSON schema,
change the severity values, ask for non-JSON output, or rename the
response fields. Doing any of those would break the parser. Use the
suffix only for guidance — keep the schema as is.

| Flag                          | Env var                       | Default                          |
|-------------------------------|-------------------------------|----------------------------------|
| `--gitlab-url`                | `GITLAB_URL`                  | `https://gitlab.com`             |
| `--gitlab-token`              | `GITLAB_TOKEN`                | (required)                       |
| `--llm-url`                   | `LLM_URL`                     | `http://localhost:11434/v1`      |
| `--llm-api-key`               | `LLM_API_KEY`                 | empty (Ollama ignores)           |
| `--llm-model`                 | `LLM_MODEL`                   | `qwen2.5-coder:7b`               |
| `--bot-username`              | `GITLAB_BOT_USERNAME`         | empty (all comments count)       |
| `--webhook-secret` (serve)    | `GITLAB_WEBHOOK_SECRET`       | (required)                       |
| `--max-diff-bytes`            | `MREVIEW_MAX_DIFF_BYTES`      | `200000`                         |
| `--temperature`               | `MREVIEW_TEMPERATURE`         | `0.2`                            |
| `--max-tokens`                | `MREVIEW_MAX_TOKENS`          | `2048`                           |
| `--per-chunk-timeout`         | `MREVIEW_PER_CHUNK_TIMEOUT`   | `120s`                           |
| `--queue-size` (serve)        | `MREVIEW_QUEUE_SIZE`          | `32`                             |
| `--addr` (serve)              | `MREVIEW_ADDR`                | `:8080`                          |
| `--shutdown-timeout` (serve)  | `MREVIEW_SHUTDOWN_TIMEOUT`    | `30s`                            |
| `--retries`                   | `MREVIEW_RETRIES`            | `3`                              |
| `--retry-backoff`             | `MREVIEW_RETRY_BACKOFF`       | `500ms`                          |
| `--config` (global)           | `MREVIEW_CONFIG`              | `~/.config/mreview/config.yaml`  |

## Architecture

```
┌──────────────┐  ┌────────────────────┐  ┌──────────────────┐
│ mreview serve │──│  HTTP /webhook      │──│  Worker pool      │
│              │  │  verify X-Gitlab-    │  │  (4 goroutines,   │
│ GitLab       │  │  Token HMAC          │  │  bounded queue)   │
│ sends MR     │  └────────────────────┘  └────────┬─────────┘
│ events here  │                                     │
└──────────────┘                                     │
                                                    ▼
                          ┌──────────────────────────────────┐
                          │  Reviewer.ReviewMR              │
                          │  fetch → chunk → LLM → dedupe →  │
                          │  post                            │
                          └──────────┬───────────────────────┘
                                     │
                                     ▼
                          ┌──────────────────────────────────┐
                          │  Local LLM (Ollama / llama.cpp /  │
                          │  vLLM / LM Studio)                │
                          └──────────────────────────────────┘
```

`mreview review` is the same pipeline but skips the webhook front-end;
it's the right entrypoint for CI jobs, cron, and `make review-MR123`.

### Package layout

```
cmd/mreview/             CLI wiring (kong), exit codes, slog setup,
                          subcommand dispatch
internal/gitlab/          Typed wrapper around client-go:
                          FetchMR, FetchChanges, PostSummary,
                          PostDiscussion, ListDiscussions, CurrentUser
                          + retry/backoff/typed errors
internal/llm/             OpenAI-compatible Provider + resilient
                          JSON parser + per-file chunker + prompt
                          templates
internal/reviewer/        Orchestrator: fetch → chunk → LLM → parse →
                          dedupe → post. End-to-end ReviewMR.
internal/server/          Webhook HTTP receiver + bounded worker pool,
                          constant-time token verify, graceful shutdown
internal/logging/         slog JSON/text setup
internal/config/          YAML config loader (--config file flag)
examples/                 Sample config + docker-compose + gitlab-ci
Dockerfile{,debug}        distroless static, multi-arch via goreleaser
.goreleaser.yaml          Builds, archives, signs, docker images
.github/workflows/        ci (lint + test on every push) + release (tag)
```

### Why this layering

**Three separate GitLab/Reviewer/Server packages.** `internal/gitlab/`
knows how to talk to GitLab and nothing else. `internal/reviewer/`
knows the review pipeline and depends on `gitlab.Client` only as a
typed interface — it never imports `client-go`. `internal/server/`
knows how to receive a webhook, enforce auth + throttling, and
fan out to a worker pool. None of the three knows about the other
two. You can replace any one without touching the others: drop in a
new LLM provider without touching GitLab code; replace the worker
pool with a queue without touching the reviewer; change the webhook
auth without touching the pipeline.

**One LLM call per chunk instead of one mega-call.** Local LLMs have
small context windows and large diffs blow past them. We chunk by
file (with hunk-level splitting for huge files) so each prompt fits
the model. The cost is more LLM calls — for a 500-line MR across
5 files, that's 5 chunk calls plus 1 merge call. The win is
coverage: no file is silently truncated because the diff was too
big.

**The merge verdict is a separate LLM call.** After per-chunk
reviews, we call the LLM one more time with all per-chunk summaries
+ the combined findings list, asking it to write a single summary
paragraph. This avoids the "summary only describes the last chunk
the model saw" problem — the LLM that wrote the per-chunk reviews
doesn't have the cross-chunk context, so the consolidate step
gives it. If the merge call fails (transient), we fall back to
concatenating per-chunk summaries so the user still gets something.

**Body-hash dedupe, not (file, line, body).** Re-running the bot on
an unchanged MR should be a no-op. We use SHA-256 of the
normalized finding body as the fingerprint. This is robust to
trivial edits (extra whitespace, line-wrapping) but a known
weakness: a substantive rewrite of the same finding produces a
different fingerprint and the bot posts a near-duplicate. The
plan to fix this involves parsing `note.position.{new_path,
new_line}` out of GitLab's discussions API and combining into the
fingerprint — deferred until users complain about duplicate noise.

**Constant-time HMAC compare.** Webhook auth uses
`crypto/subtle.ConstantTimeCompare` so the secret doesn't leak
byte-by-byte through timing. We also compare against a same-length
string when lengths differ, so the wall-clock cost is independent
of whether the lengths match.

**Bounded worker pool.** Four goroutines, channel-buffered queue,
HTTP 503 on queue-full. GitLab retries 5xx with exponential
backoff, so saturation signals "try again later" without losing
events. A per-MR webhook throttle (default 30 s) prevents
rapid-fire duplicate deliveries from queueing at all.

**`context.Context` everywhere.** Every blocking call in the
reviewer and the server takes a context. SIGINT/SIGTERM cancels
the root context, the worker pool drains in-flight jobs, the HTTP
server does `Shutdown(ctx)` for graceful drain, and any in-flight
LLM call (which is the longest pole) gets a `request canceled`
signal so we don't keep burning GPU on a request the operator
just killed.

## Performance & cost

Numbers below are typical — actuals depend on the model, the
hardware it runs on, and the shape of your MRs.

### Token usage

The prompt uses a chars/4 heuristic to estimate tokens (no
per-model tokenizer — keep the binary small). Rough per-review
budget for a **500-line MR across 5 files** with the default
`--max-diff-bytes=200000` (i.e. one chunk per file):

| Component | Tokens (input) | Tokens (output) |
|---|---|---|
| Per-chunk review (×5) | ~2 500 each = 12 500 | ~500 each = 2 500 |
| Merge verdict | ~3 000 | ~500 |
| **Total per review** | **~15 500** | **~3 000** |

Output tokens dominate cost on most local-LLM setups (where
"cost" = GPU-seconds). A 7B model at Q4 quantization on a 4090
generates ~80 tokens/s for code; a 13B at Q4 on the same GPU is
~40 tokens/s. Bigger MRs scale roughly linearly: a 5 000-line MR
across 50 files is roughly 10× the token budget.

### Latency

| Stage | Typical time |
|---|---|
| GitLab API calls (3 calls per review: fetch MR, fetch changes, list discussions) | < 1 s total |
| Per-chunk LLM call (7B model on decent GPU/CPU) | 5 – 30 s |
| Per-chunk LLM call (CPU-only Ollama, 7B Q4) | 30 – 120 s |
| Merge verdict call | Same as per-chunk |
| Posting comments (1 summary + N inline findings) | < 1 s total |

For a typical 5-file MR on GPU-accelerated Ollama, end-to-end
review time is **30 – 90 seconds**. On a CPU-only Raspberry Pi 5
with a 7B model, expect **5 – 15 minutes** — the bot will hit
its per-chunk timeout (default 120 s) on slow chunks. Bump
`--per-chunk-timeout` (or `--max-diff-bytes` to keep prompts
smaller) for slow setups.

### Memory

**mreview binary:** ~20 MB static, ~50 MB RSS at idle. Concurrent
reviews each consume ~10 MB transient (mostly the diff in memory
during chunking).

**LLM server (Ollama example):**

| Model | RAM (inference) |
|---|---|
| 7B Q4 | ~5 GB |
| 13B Q4 | ~9 GB |
| 70B Q4 | ~40 GB |

KV cache grows with context length — a 32 k context window adds
1–4 GB on top of the model weights. For long-MR chunked reviews
this matters: if your LLM OOMs on a chunk, raise `--max-diff-bytes`
(or `--max-tokens`) to keep prompts shorter.

### GitLab API rate limits

`mreview review` makes 3–5 GitLab API calls per run:

- `GET /merge_requests/:iid` (1)
- `GET /merge_requests/:iid/changes` (1)
- `GET /merge_requests/:iid/discussions` (1, dedupe baseline)
- `POST /merge_requests/:iid/notes` (0 or 1, summary)
- `POST /merge_requests/:iid/discussions` (1 per inline finding)

GitLab.com's per-user rate limit is generous for `api`-scoped
PATs (2 000 requests/hour). `mreview serve` running on a busy
instance (say, 50 MRs/hour) consumes ~250 requests/hour — well
under the cap. Self-hosted GitLab has no enforced rate limit.

`mreview` does its own retry on transient 5xx / 429 via
`internal/gitlab/retry.go`. The `Retry-After` HTTP header is
honored up to a 60-second cap (longer values get clipped).

### When to upgrade

| Symptom | Fix |
|---|---|
| Reviews take minutes per MR | Smaller model (7B → 3B); faster hardware; lower `--max-diff-bytes` |
| Frequent "queue full" 503s on `serve` | Raise `--queue-size`; scale up `mreview serve` replicas (each is stateless — they don't share dedupe state today, which is a known limitation, see [Issue: shared dedupe store](#) for the future fix) |
| Bot posts near-duplicate findings across pushes | Switch dedupe to `(file, line, body)` (deferred) |
| LLM OOMs on a chunk | Lower `--max-diff-bytes`; use a smaller context window |

## Development

### One-time bootstrap

```bash
lefthook install          # enable pre-commit (lint + test)
```

### Day-to-day

```bash
go test -race -coverprofile=coverage.out ./...   # run all tests with race
go vet ./...                                    # static checks
golangci-lint run                               # full lint
go build -trimpath -ldflags="-s -w" -o dist/mreview ./cmd/mreview
go mod tidy
```

The `lefthook` pre-commit hook runs `golangci-lint run` and
`go test -race -count=1 ./...` on staged changes.

### TDD workflow

Every change follows: red (failing test) → green (impl) → refactor →
tests green → lint clean → conventional commit. Commit messages follow
[Conventional Commits](https://www.conventionalcommits.org/) —
`feat:`, `fix:`, `test:`, `docs:`, `refactor:`, `chore:`, `build:`, `ci:`.

### Releasing

```bash
# Tag a release. CI picks it up and runs goreleaser.
git tag v0.1.0
git push origin v0.1.0

# Pre-release tags (vX.Y.Z-rc.N, vX.Y.Z-beta.N) also trigger the
# release workflow but are explicitly intended as unstable.
```

## Troubleshooting

### "auth (401)" against GitLab

The Personal Access Token is missing, expired, or under-scoped. Verify
it at <https://gitlab.com/-/user_settings/personal_access_tokens> and
ensure it has the `api` scope (not `read_api`).

```bash
# Quick sanity check:
GITLAB_TOKEN=glpat-xxx mreview doctor --skip-llm
```

### "transient failure (retries exhausted)" on every inline discussion (summary still posts)

The classic symptom is `findings_total=N findings_posted=0 summary_posted=true` — every inline discussion fails after three retries with HTTP 500, but the summary note posts fine. Both endpoints are authenticated and authorized the same way, so the failure mode is endpoint-specific.

**Root cause**: GitLab's `/discussions` endpoint requires position fields to match the file's diff status. From the [API docs](https://docs.gitlab.com/api/discussions/#create-new-merge-request-thread):

> Both `position[old_path]` and `position[new_path]` are required and must refer to the file path before and after the change.
>
> To create a thread on an added line (highlighted in green in the merge request diff), use `position[new_line]` and don't include `position[old_line]`.

A comment anchored to the **new side** of the diff must send `new_path + new_line` only; a comment on a removed line or deleted file must send `old_path + old_line` only. Sending `old_line: 0` (or empty `old_path: ""`) makes GitLab's internal diff-line lookup fail with a generic 500 — there's no line 0 to anchor against.

mreview's reviewer uses the diff metadata from the chunker (`ChangeFile.IsNew`, `IsDeleted`, `OldPath`, `NewPath`) to construct the right position shape per file status. If you're seeing this error on an older release, upgrade — the fix landed in v0.3.1.

If you're still seeing this on the latest release, share the relevant GitLab `production.log` stack trace for the 500; it will name which position field GitLab's parser is choking on.

### "list models: ..." or "ping: ..." failures in `mreview doctor`

The LLM endpoint isn't reachable. For Ollama: ensure `ollama serve` is
running and `qwen2.5-coder:7b` (or whichever model) is pulled
(`ollama pull qwen2.5-coder:7b`). For llama.cpp / vLLM / LM Studio,
verify the server is listening on the configured `--llm-url`.

### "diff too large to chunk: ..."

A single file's diff exceeds `--max-diff-bytes` (default 200 KB). Options:

- Raise the budget: `--max-diff-bytes=500000`.
- Split the MR (smaller MRs = better reviews regardless).
- File an issue with the diff and let a human review the offending file.

### "line out of range" warnings in logs

The LLM cited a line number that's beyond the file's length — the LLM
hallucinated, the file has moved since the LLM saw it, or the file was
renamed. The reviewer drops that single finding and continues with the
rest. No action needed; if it happens often, consider a smaller model
or a tighter prompt.

### "queue full" HTTP 503 from `mreview serve`

The worker pool is saturated (slow LLM + many simultaneous MR events).
Solutions:

- Raise `--queue-size` (default 32).
- Raise `--shutdown-timeout` so the pool can drain on the next deploy.
- GitLab retries 5xx with exponential backoff, so no events are lost.

## Examples

The `examples/` directory ships with copy-pasteable starting points.
Most operators only need one or two.

| File | When you'd use it |
|---|---|
| [`config.yaml`](examples/config.yaml) | Persist flags + env across invocations — every secret references an env-var NAME (never a value). Drop at `~/.config/mreview/config.yaml` or pass with `--config`. |
| [`docker-compose.yml`](examples/docker-compose.yml) | Local dev or single-host production: brings up Ollama + mreview with health checks and a named volume for the model. |
| [`gitlab-ci.yml`](examples/gitlab-ci.yml) | CI-driven review instead of a long-running `serve` — runs on every MR pipeline. Best for teams that already pay for CI minutes and prefer ephemeral review jobs. |
| [`webhook-setup.md`](examples/webhook-setup.md) | Walkthrough of the GitLab UI to install the webhook for one project, with secret-rotation + HTTPS security notes. |
| [`team-prompt-system.txt`](examples/team-prompt-system.txt) | Operator-supplied text appended to **every** system prompt — documentation standards, dependency preferences, severity semantics. Always-loaded team conventions. |
| [`team-prompt-user.txt`](examples/team-prompt-user.txt) | Operator-supplied text appended to **every** user prompt — per-MR context the LLM should weigh heavily ("this is a dep bump", "focus on architecture"). |

A typical setup uses **two**: copy `config.yaml` for persistent
connection settings and one of `team-prompt-system.txt` /
`team-prompt-user.txt` for the LLM guidance. Teams that already
have CI add `gitlab-ci.yml`. Teams without CI infrastructure use
`docker-compose.yml` + the webhook setup guide.

## License

GNU Affero General Public License v3.0 or later — see [LICENSE](LICENSE).

This is **copyleft** software with the AGPL network clause: anyone is
free to use, modify, and redistribute mreview, but any derivative
work distributed to others (including as a network service) must
also be released under AGPL-3.0-or-later. **If you run a modified
mreview as a service that others interact with over a network,
you must provide the source of your modifications to those users.**

That last clause is the practical difference vs plain GPL-3.0:
mreview running as a SaaS that customers talk to owes those
customers the source.

For the full text, see <https://www.gnu.org/licenses/agpl-3.0.html>.

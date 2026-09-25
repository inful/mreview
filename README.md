# mreview

A thin GitLab-CI review orchestrator. mreview fetches the diff for a
merge request, hands it to a [harness](https://github.com/sausheong/harness)-
driven LLM agent (with [tokensave](https://tokensave.dev/) as its primary
code-graph tool), and posts the result back to the MR as inline line
comments plus a summary thread.

Architecture reset as of v0.6.0 ([#42](https://github.com/inful/mreview/issues/42)):
mreview is now a CI-only orchestrator with a strictly read-only agent
tool surface. The `mreview serve` mode is dropped; CI is the canonical
run mode, and the central CI definition (the example in
`examples/central-ci.yml`) handles onboarding.

```text
$ ./mreview review --repo=group/project --mr=42 --provider=local \
    --provider-base-url=http://localhost:11434/v1 \
    --workdir=$PWD --artifacts-dir=.mreview-artifacts \
    --tokensave-enabled=true --log-format=json
{"time":"...","level":"INFO","msg":"per-event guard","source":"merge_request_event"}
{"time":"...","level":"INFO","msg":"policy loaded","severity_overrides":2}
{"time":"...","level":"INFO","msg":"artifacts loaded","build":"present","tests":"missing"}
{"time":"...","level":"INFO","msg":"starting review","repo":"group/project","mr":42,"provider":"local"}
{"time":"...","level":"INFO","msg":"running review agent","system_prompt_bytes":3217,"user_prompt_bytes":1894}
{"time":"...","level":"INFO","msg":"review complete","findings":2,"summary_posted":true,"policy_error":false}
https://gitlab.example.com/group/project/-/merge_requests/42
```

## Table of contents

- [Why mreview](#why-mreview)
- [How it works](#how-it-works)
- [Install](#install)
- [Quickstart](#quickstart)
- [Subcommands](#subcommands)
- [Behaviour by event](#behaviour-by-event)
- [Policy enforcement](#policy-enforcement)
- [Read-only tool surface](#read-only-tool-surface)
- [CI artifact reuse](#ci-artifact-reuse)
- [Skills](#skills)
- [What the bot posts](#what-the-bot-posts)
- [Exit codes](#exit-codes)
- [Read-only tool surface](#read-only-tool-surface)
- [Tokensave bundle](#tokensave-bundle)
- [CI artifact reuse](#ci-artifact-reuse)
- [Configuration](#configuration)
- [Onboarding via central CI](#onboarding-via-central-ci)
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

- **CI-first, single-binary.** One `mreview review` invocation per MR,
  driven by your central CI template. No long-lived daemon, no
  webhook receiver, no per-repo wiring.
- **Strictly read-only agent.** The harness agent has access to
  `read_file` plus the [tokensave](https://tokensave.dev/) MCP server
  for code-graph queries — nothing else. No shell, no write tools, no
  re-running CI commands. The contract is enforced by tests in
  `internal/reviewer/readonly_test.go` and
  `internal/prompts/review_test.go`.
- **Language-agnostic.** tokensave handles symbol extraction, blast
  radius, and semantic search across 50+ languages. mreview never
  reads source files just to parse them.
- **Multi-provider.** Six LLM providers wired through
  [harness](https://github.com/sausheong/harness): Anthropic,
  OpenAI, Gemini, LiteLLM, OpenRouter, and local Ollama/LM Studio.
  `--provider=…` selects; the harness library owns the wire format.
- **CI artifact reuse.** Build, test, lint, and vulncheck are run
  *before* mreview by separate CI stages. mreview reads their
  artifacts and injects them into the harness prompt as pre-loaded
  context — the agent never re-runs them.
- **Policy-driven.** A `policy.yaml` file controls how findings are
  severity-escalated, what synthetic rules fire, and what label
  triggers an MR-wide escalation. Missing / empty / malformed
  artifacts degrade the review gracefully, never crash it.

## How it works

```
┌──────────────────────────────────────────────────────────────────┐
│  Central CI pipeline (one stage per producer + one for mreview)  │
│                                                                  │
│   go-build      ──┐                                             │
│   go-test       ──┤                                             │
│   golangci-lint  ─┼─→ .mreview-artifacts/                       │
│   govulncheck   ──┤   build.log                                  │
│   tokensave sync ┘   test_results.json                          │
│                     lint.json                                   │
│                     vulns.json                                   │
│                                                                  │
│                                  ┌─────────────────────────┐    │
│   mreview  ←────────────────────┤  Harness Runtime         │    │
│   (orchestrator + reviewer)      │                         │    │
│                                  │  Provider:              │    │
│   1. Read CI artifacts  ────────► │   Anthropic / OpenAI /  │    │
│                                  │   Gemini / LiteLLM /    │    │
│   2. Read policy.yaml ──────────► │   OpenRouter / Local    │    │
│                                  │                         │    │
│   3. Fetch MR + diff from  ────► │  Tools:                  │    │
│      GitLab (gitlab client)      │   read_file              │    │
│                                  │   mcp__tokensave__*      │    │
│   4. Build user prompt:          │     smart_context        │    │
│      MR metadata + diff          │     semantic_search      │    │
│      chunks + artifact           │     impact_analysis      │    │
│      block + policy hints        │                         │    │
│                                  │  Loop:                   │    │
│   5. Run harness ────────────────► │   think → tool call →   │    │
│                                  │   think → ... → emit    │    │
│   6. Apply policy.Enforce()  ◄───│   findings JSON         │    │
│   7. Dedupe vs existing     ◄────┘                         │    │
│      GitLab discussions                                         │
│   8. Post summary + inline  ────► GitLab API                  │
│      findings                                                  │
└──────────────────────────────────────────────────────────────────┘
```

The orchestrator (`internal/reviewer/orchestrator.go`) is a pure
Go function — no goroutines, no plugin discovery, no event loop.
Context cancellation propagates. The harness library owns the LLM
loop, streaming, prompt caching, MCP connection lifecycle, and
session persistence; mreview owns the GitLab plumbing, the policy
enforcer, the artifact loader, and the orchestrator glue.

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

The container image bundles both `mreview` and `tokensave` so
the example CI templates work end-to-end with no extra setup.
See [Tokensave bundle](#tokensave-bundle) for the version pin,
checksums, and the `--tokensave-bin` escape hatch.

### `go install`

```bash
go install github.com/inful/mreview/cmd/mreview@latest
```

## Quickstart

```bash
# 1. Verify your wiring before posting anything to GitLab.
mreview doctor --skip-provider   # GitLab check (needs $GITLAB_TOKEN)
mreview doctor --skip-gitlab     # provider check (needs running Ollama / etc.)

# 2. Dry-run a real review. Logs every harness call + GitLab
#    post it WOULD make, without actually posting. Cheap, safe, fast.
GITLAB_TOKEN=glpat-xxx \
mreview review \
    --repo=group/project \
    --mr=42 \
    --provider=local \
    --provider-base-url=http://localhost:11434/v1 \
    --workdir=$PWD \
    --dry-run \
    --log-format=json

# 3. Same command, no --dry-run. Comments land on the MR.
GITLAB_TOKEN=glpat-xxx \
mreview review --repo=group/project --mr=42 \
    --provider=local \
    --provider-base-url=http://localhost:11434/v1 \
    --workdir=$PWD

# 4. CI invocation (the canonical mode). Drop the reference
#    central CI template into your org's CI library repo; the
#    mreview job runs on MR open / reopen / push.
#    See examples/central-ci.yml for the full producer + mreview
#    pipeline layout.

# 5. Or persist the connection settings in a config file so
#    every invocation doesn't repeat them.
mkdir -p ~/.config/mreview
cp examples/config.yaml ~/.config/mreview/config.yaml
$EDITOR ~/.config/mreview/config.yaml    # set gitlab.url + provider.base_url
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
      --provider=STRING                          LLM provider (anthropic / openai / gemini /
                                                   litellm / openrouter / local). Default "local".
      --provider-base-url=STRING                 Provider base URL (required for litellm / local;
                                                   ignored for hosted providers).
      --model=STRING                             Model name (provider-specific).
      --workdir=STRING                           Working directory the harness runs against
                                                   (defaults to the repo root).
      --tokensave-enabled=true                   Enable the tokensave MCP server (the agent's
                                                   primary code-graph tool). Default true.
      --tokensave-bin=STRING                     Path to the tokensave binary (default: PATH-resolved
                                                   'tokensave'). Used when --tokensave-enabled=true.
      --artifacts-dir=STRING                     Directory containing CI artifacts (build.log,
                                                   test_results.json, lint.json, vulns.json).
                                                   Default .mreview-artifacts.
      --policy-file=STRING                       Path to a YAML policy file (severity_overrides,
                                                   forbid, require, labels). Empty = no policy.
                                                   See "Policy enforcement" below for the schema.
      --on-drafts=skip                           Action on draft MRs (CI_MERGE_REQUEST_DRAFT=true):
                                                   run the review or skip with exit 0. Default skip.
      --on-push=skip                             Action on direct branch pushes
                                                   (CI_PIPELINE_SOURCE=push): run the review or
                                                   skip with exit 0. Default skip.
      --bot-username=STRING                      Bot username for dedupe ($GITLAB_BOT_USERNAME).
      --retries=3                                GitLab API retry attempts on transient errors.
      --retry-backoff=500ms                      Initial retry backoff; exponential with jitter.
      --dry-run                                  Log intended GitLab posts without performing them.
      --log-format=text                          Log output format (text | json).
      --verbose                                  Enable debug logging.
      --config=STRING                            Path to YAML config file (default ~/.config/mreview/config.yaml).
```

The GitLab token must have `api` scope. The provider's API key is
read from the provider-specific env var (e.g. `ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`); the harness library handles the convention.

### `mreview doctor`

Validate config + provider + GitLab connectivity. Useful in CI / pre-deploy.

```text
Usage: mreview doctor [flags]

Flags:
      --gitlab-url=...                           (same as review)
      --gitlab-token=STRING                      ($GITLAB_TOKEN)
      --provider=STRING                          (same as review)
      --provider-base-url=STRING                 (same as review)
      --model=STRING                             (same as review)
      --skip-gitlab                              Skip the GitLab check.
      --skip-provider                            Skip the provider check.
```

Example output:

```text
mreview doctor
─────────────
GitLab: ok  alice
  URL: https://gitlab.com
  Name: Alice Example

Provider: ok  local ready (model=qwen2.5-coder:7b, 3 known models)
  Models: qwen2.5-coder:7b, llama3:8b, mistral:7b

Config: ok  provider=local model=qwen2.5-coder:7b

All checks passed.
```

On failure (e.g. wrong token, unreachable provider) the subcommand
exits 1 with a per-check breakdown.

### `mreview serve` — removed

The `mreview serve` webhook-receiver mode was dropped in the
architecture reset ([#42](https://github.com/inful/mreview/issues/42)).
CI is the canonical run mode; central CI definitions handle
onboarding (see [Onboarding via central CI](#onboarding-via-central-ci)
below). Operator-facing migration notes are in [issue #42's
comment thread](https://github.com/inful/mreview/issues/42).

## Behaviour by event

When invoked from a GitLab CI pipeline, `mreview review` reads the
predefined `CI_PIPELINE_SOURCE` (and, for MR events, `CI_MERGE_REQUEST_DRAFT`)
to decide whether the event warrants a review. The supported / unsupported
table mirrors GitLab's [predefined CI variables][gitlab-ci-vars]:

| `CI_PIPELINE_SOURCE`         | Default behaviour                | Override        |
|------------------------------|----------------------------------|-----------------|
| _empty (local CLI invocation)_ | Run review                      | —               |
| `merge_request_event` (non-draft) | Run review                  | —               |
| `merge_request_event` (draft)    | **Skip** (exit 0)            | `--on-drafts=run` |
| `web` / `api` / `schedule`   | Run review                      | —               |
| `push`                       | **Skip** (exit 0)                | `--on-push=run`  |
| `trigger` / `pipeline` / `parent_pipeline` | **Skip** (exit 0) — would cause feedback loops | — |
| `webide` / `chat` / `ondemand_dast_*` / `security_orchestration_policy` / `external_pull_request_event` | **Skip** (exit 0) | — |

[gitlab-ci-vars]: https://docs.gitlab.com/ee/ci/variables/predefined_variables.html

**Skip means a clean exit 0.** CI runners treat the no-op as success;
the guard emits a one-line `Debug`-level log line with the reason
(use `--verbose` to see it).

**Why a guard at all?** mreview supports both CI and `mreview serve`
modes today. When the project moves to CI-first review (per the
[architecture reset](https://github.com/inful/mreview/issues/42)),
a stray invocation from a `trigger:` child pipeline or a `webide`
launch would otherwise spam reviews on every code edit. The guard
short-circuits those paths with a deterministic `exit 0`.

**Local invocation.** When `CI_PIPELINE_SOURCE` is unset (a developer
running `mreview review` from a terminal), the guard always proceeds —
even if `--on-drafts=skip` is set, that flag is a no-op locally. The
override flags exist for CI-only behaviour; they don't change local
ergonomics.

The per-event *content* of the review (incremental vs full, summary
post, dedupe baseline) is the orchestrator's concern — see `internal/reviewer/`.

## Policy enforcement

A YAML policy file (passed via `--policy-file` / `$MREVIEW_POLICY_FILE`)
escalates or rejects findings before they're posted to GitLab.
The schema is locked in by [issue #42's migration step 2][issue-42]
(architecture reset) — adding new sections is fine, renaming or
removing existing ones is a breaking change.

```yaml
# policy.yaml — escalation and synthesis rules
severity_overrides:
  - pattern: "**/*.go"
    severity: error          # any finding on .go files becomes error
  - pattern: "internal/security/**"
    severity: error

forbid:
  - id: no-todo-comments
    pattern: "TODO"
    message: "TODO comments are not allowed in main"
  - id: no-fmt-prints
    pattern: 'fmt\.Print(ln)?\('
    message: "use the structured logger (slog) instead of fmt.Print*"

require:
  - id: has-tests
    pattern: "internal/**/*_test.go"
    message: "MRs touching internal/ must include a test file"

labels:
  "security-review": error    # when the MR has this label, escalate everything
  "breaking-change": error
```

[issue-42]: https://github.com/inful/mreview/issues/42

**Behaviour:**

| Section             | Effect                                                                                |
|---------------------|---------------------------------------------------------------------------------------|
| `severity_overrides` | Escalates findings whose `file` matches the doublestar glob to the configured severity. First match wins. Weaker overrides don't downgrade. |
| `forbid`            | Regex match against the content of every file in the MR. On match, adds a synthetic `error` finding. |
| `require`           | Doublestar glob must match at least one path in the MR. On miss, adds a synthetic `error` finding. |
| `labels`            | When the MR carries one of these labels (from `CI_MERGE_REQUEST_LABELS`), every finding's verdict is escalated to the configured severity. Strongest escalation wins. |

**Exit-code semantics:**

- Any `error`-verdict finding (or any `forbid` / `require` rule firing) → the review exits with code **`8` (`ExitPolicy`)**, distinct from `7` (`ExitInternal`, "review crashed").
- `warning`-verdict findings surface on the MR but don't fail the review.
- `info`-verdict findings are logged.

**Validation:**

The file is loaded and validated at startup — unknown top-level
fields, invalid glob/regex patterns, bad severity values, and
missing required IDs all return `ExitConfig` (2) before any
GitLab / harness work happens.

See [`internal/policy/`](internal/policy/) for the schema types
and the [`examples/policy.yaml`](examples/policy.yaml) reference.

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
- Any chunk's LLM call fails after the retry budget is exhausted.
  By default (`--allow-partial=false`) the whole review aborts
  with exit code 7 — no summary note, no inline discussion. The
  log carries the batch index, file list, and underlying error;
  operators re-run rather than trusting a half-completed report.
  Pass `--allow-partial=true` to restore the legacy "log a warn
  and substitute empty findings" path, useful on big MRs where
  one bad chunk isn't worth aborting.

Operators running with `--dry-run` see every post *attempted* in
the logs (with body and URL) without anything actually landing on
GitLab — use this to verify what the bot will say before letting it
hit a real MR.

## Exit codes

| Code | Meaning                                          |
|------|--------------------------------------------------|
| `0`  | Success                                          |
| `2`  | Configuration error (missing flag, bad flag, missing config, malformed policy file) |
| `3`  | Authentication error (GitLab rejected the token) |
| `4`  | Not found (MR, project)                          |
| `5`  | Conflict (line anchor out of range, stale MR head) |
| `6`  | Transient failure exhausted (5xx / 429 retries)  |
| `7`  | Unexpected internal error                        |
| `8`  | Policy violation — at least one `error`-verdict finding (or any `forbid` / `require` rule fired) per `policy.yaml`. Distinct from `7` so CI scripts can tell "policy said no" apart from "review crashed." |

CI scripts can branch on these. `mreview review` follows the table
above; `mreview doctor` uses 0 / 1 (it always runs to completion
so it can report failures).

## Read-only tool surface

The harness agent has **exactly** these tools registered:

| Tool | Purpose |
|------|---------|
| `read_file` | Read raw source / config files |
| `mcp__tokensave__smart_context` | Code-graph queries ("what does this code do / what depends on it") |
| `mcp__tokensave__semantic_search` | Semantic search across the repo |
| `mcp__tokensave__impact_analysis` | Blast radius ("if I change this, what breaks") |

The agent does **not** have:

- shell / bash
- write_file / edit_file
- any tool that mutates the working directory

This is the read-only contract enforced by tests in
`internal/reviewer/readonly_test.go` and
`internal/prompts/review_test.go`. The harness library's bash /
edit_file / write_file tools are imported only by tests that
prove they aren't registered; mreview never references them
in production code.

## Tokensave bundle

`mreview` is only useful when the tokensave MCP server can
connect. The container image (`ghcr.io/inful/mreview:*`) bundles
the binary at `/usr/local/bin/tokensave` so the example CI
templates work out of the box — without this, the harness
spawn fails inside `distroless/static` (no `tokensave` on
`$PATH`) and the agent silently falls back to read_file-only
mode.

### What's bundled

| Component       | Version | Path in image                  | License      |
|-----------------|---------|--------------------------------|--------------|
| `mreview`       | (build) | `/mreview`                     | AGPL-3.0     |
| `tokensave`     | 7.12.1  | `/usr/local/bin/tokensave`     | MIT          |

The third-party MIT notice ships in each release archive as
`NOTICE-tokensave.txt`.

### Version pin

The version is pinned in `Dockerfile` (and `Dockerfile.debug`)
via build args:

```dockerfile
ARG TOKENSAVE_VERSION=7.12.1
ARG TOKENSAVE_SHA_AMD64=184612db16800e384a1bcdc7fadcc53fa73bda70240f9c0416ac4b88c7e924fb
ARG TOKENSAVE_SHA_ARM64=7de5c95b51d508f39a83d9420ad0700502de107a61dbb38dad3d008d4f6f4261
```

To roll forward, update the three values together (Linux/amd64
and Linux/arm64 checksums) and the same SHA entries in
`scripts/tokensave-smoke.sh`. Pull the new checksums from the
upstream [`SHA256SUMS`](https://github.com/aovestdipaperino/tokensave/releases/latest)
asset — **never** compute them from a freshly-downloaded
archive (fail-closed, same policy as `tokensave upgrade`).
Avoid v7.12.0; its release pipeline was broken and the tag was
withdrawn.

### Contract smoke test

`scripts/tokensave-smoke.sh` verifies three things every CI
run, before the image gets built:

1. The pinned release URL serves an archive matching the
   pinned SHA256 (so the Dockerfile build wouldn't be
   downloading a stale asset).
2. The extracted binary runs (`tokensave --version`).
3. The MCP subcommand is `serve` (not `mcp`) and accepts
   `--path <project>` — the shape `mreview` passes to it
   from `internal/tokensave/mcp.go`.

This caught a real drift: an earlier version of this code
spawned `tokensave mcp --project-root <path>`, which the
v7.12.x CLI no longer accepts. The smoke test now blocks the
build until the contract is corrected, and the contract smoke
is wired into `.github/workflows/ci.yml` as the
`tokensave-smoke` job.

### Escape hatch: `--tokensave-bin`

If you need a different tokensave build (newer release,
self-hosted fork, locally-built debug binary), mount it into
the container and point mreview at it:

```bash
docker run --rm \
  -v /path/to/your-tokensave:/usr/local/bin/tokensave:ro \
  ghcr.io/inful/mreview:latest \
  review --tokensave-bin=/usr/local/bin/tokensave ...
```

The flag accepts an absolute path; mreview execs it directly
without `PATH` lookup. Disable the MCP server entirely with
`--tokensave-enabled=false` (the agent falls back to
read_file-only mode; useful when tokensave is unsuitable for
the repo, e.g. a vendored tree too large to index).

### `tokensave-sync` still runs separately

The CI pipeline's `tokensave-sync` stage (see
`examples/central-ci.yml`) is **not** replaced by the bundled
binary. It does one-shot indexing (`tokensave sync`) that
primes the `.tokensave/` cache; the bundled binary is the MCP
server that runs at query time. The sync stage image and the
bundled version should be the same release — when you bump one,
bump the other.

## CI artifact reuse

The central CI pipeline runs build / test / lint / vulncheck
**before** mreview, then writes their output to
`.mreview-artifacts/`:

| Artifact | Source |
|----------|--------|
| `build.log` | `go build ./...` (tail of the log + build-error detection) |
| `test_results.json` | `go test -json -race -count=1 ./...` (NDJSON) |
| `lint.json` | `golangci-lint run --out-format=json ./...` (JSON array) |
| `vulns.json` | `govulncheck ./...` (JSON) |

mreview reads these at startup (via `--artifacts-dir`,
default `.mreview-artifacts`) and injects them into the
harness prompt as pre-loaded context. The agent never
re-runs the producer tools — it has no shell access.

**Robustness contract:** missing / empty / malformed
artifacts cause **degraded information**, not errors. The
prompt explicitly tells the agent which artifacts are
`NOT AVAILABLE` so it can self-calibrate confidence. The
review always runs; the artifact availability only changes
how much corroboration the agent has.

See [`examples/central-ci.yml`](examples/central-ci.yml) for
the full producer + mreview pipeline layout.

## Skills

Skills are team-authored review guidance (`.md` files) the
agent reads on demand via two MCP tools:

| Tool | Returns |
|---|---|
| `mcp__skills__list_skills` | Every skill's name, description, and source path. No body bytes. |
| `mcp__skills__read_skill` | The full body of one skill by name. |

The agent calls `list_skills` once after reading the diff
headers, then `read_skill` for each skill whose description
matches what it sees (language, framework, file kind). Skill
bodies are advisory — they don't override the read-only
contract or the Finding schema. They cover team conventions
("our error wrapping is `%w`"), language-specific patterns,
and known pitfalls.

### Two layers: bundled + central

mreview ships with a bundled set of skills that are always
available, regardless of operator configuration. The bundled
set is the baseline; a central skills repo augments it with
team-authored `.md` files.

| Source | Path in `list_skills` | When present |
|---|---|---|
| Bundled (built into the binary) | `bundled://<name>.md` | Always — every release ships the same set. |
| Central repo (configured via `--skills-repo`) | `skills/<name>.md` | When the operator configured a repo. |

The five bundled skills today:

- `go-review` — generic Go checklist (error wrapping, goroutine leaks, context propagation, race conditions, slice aliasing)
- `testing-patterns` — table-driven tests, sub-tests via `t.Run`, race detector in CI, no `time.Sleep`
- `error-handling` — wrap with `%w`, sentinel errors, custom error types, log-vs-return rule
- `tokensave-usage` — when to use which `mcp__tokensave__*` tool during a review
- `skill-authoring` — layout contract for `.md` files in a `skills/` directory; read this when reviewing MRs that touch a skills repo (the meta-circular check)

The full set lives at `internal/skills/bundled/*.md` in the
mreview source tree. See `internal/skills/bundled/bundled.go`
for the embed loader.

### Override semantics: central wins

When a `.md` in the central repo has the same name as a
bundled skill, **the central version wins**. The bundled
version is hidden. This means a team can patch any bundled
skill — tighten it, swap it for a stricter version, add
team-specific rules — by putting a same-named file in their
central repo. No fork of mreview required.

Override example: drop a `go-review.md` into your central
repo to replace the bundled default. The team-specific
version (e.g. "our team uses %s not %w") is what the agent
sees; the bundled one is silently discarded.

### Authoring skills for the central repo

One skill is one `.md` file in a shared GitLab repository's
`skills/` directory:

```markdown
---
title: Go review checklist
---

# When reviewing Go code

- Flag any use of `panic` outside `cmd/.../main.go`.
- ...
```

The first paragraph after frontmatter becomes the
`list_skills` description (capped at 200 chars). Keep it to
one or two sentences so the agent can scan the list cheaply.

For a complete reference layout — README, four example
skills, the auth + token config — see
[`examples/skills-repo/`](examples/skills-repo/).

### Configuring skills

Add a `skills:` block to your YAML config (or pass the
matching `--skills-*` flags):

```yaml
skills:
  repo_path: inful/mreview-skills   # group/project
  directory: skills                 # default
  ref: main                         # default; pin a SHA for reproducibility
  # token_env: GITLAB_TOKEN         # default; reuse the review token
```

When `repo_path` is empty the central layer is disabled —
only the bundled set ships. Existing deployments see no
behavior change beyond the new bundled skill names appearing
in `list_skills`.

The token defaults to `--gitlab-token-env` (typically
`GITLAB_TOKEN`); set `token_env` only when the skills repo
lives on a different GitLab instance with its own PAT.

See issue [#44](https://github.com/inful/mreview/issues/44)
for the design rationale and the agent-usage contract.

## Configuration

Three layers, in priority order (CLI flags always win):

1. **CLI flags** — e.g. `--gitlab-url=https://example.com`
2. **Environment variables** — `GITLAB_URL`, `MREVIEW_PROVIDER_BASE_URL`, etc.
   (every flag has a matching `env:` tag; see `--help` for each subcommand)
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
provider:
  base_url: http://localhost:11434/v1
  model: qwen2.5-coder:7b
```

### LLM presets — deprecated

The `llm_presets` / `llm_preset_by_model` YAML block was the
hand-rolled LLM-loop's mechanism for deriving packing budgets.
After the architecture reset ([#42](https://github.com/inful/mreview/issues/42)),
[harness](https://github.com/sausheong/harness) owns prompt-cache
discipline and the provider matrix. The preset layer is kept as a
stub so existing YAML configs keep parsing, but it's no longer
used to size the work. The harness library's own preset layer
will replace it.

## Onboarding via central CI

The architecture reset ([#42](https://github.com/inful/mreview/issues/42))
moves mreview from a per-repo webhook receiver to a per-MR CI
invocation. The onboarding pattern is:

1. **Pick a central location** for reusable CI workflows. Most
   orgs use a `ci-templates` or `.gitlab-ci.yml` library repo.
2. **Drop in the reference template.** The full pattern lives
   in [`examples/central-ci.yml`](examples/central-ci.yml) — copy
   it verbatim and adjust the image / artifact paths.
3. **Per-project wiring** is one line in the consumer repo's
   `.gitlab-ci.yml`:

   ```yaml
   include:
     - project: 'your-org/ci-templates'
       ref: main
       file: 'mreview.yml'
   ```

4. **No per-project config** beyond the GitLab token. The
   reusable workflow carries the producer stages, the
   tokensave-sync stage, and the mreview stage. Artifacts flow
   between stages automatically.
5. **Operator overrides** go in the consumer repo's
   `.gitlab-ci.yml` — e.g. `MREVIEW_PROVIDER_BASE_URL` if the
   org's LLM proxy lives at a non-default URL.

This pattern is what "the central CI definition handles
onboarding" means in the locked decisions table. mreview
ships `examples/central-ci.yml` as the *reference template*;
the actual central workflow lives wherever the org keeps it.

For teams that don't have a central CI repo yet, the reference
template can be inlined into each repo's `.gitlab-ci.yml` — it
works standalone.

Use the preset field when the model's effective context is smaller than its advertised window — common for heavily quantized local models that return empty content (rather than timing out) on prompts technically within the byte budget.

The optional `reasoning_effort` field on a preset (or the matching
`--reasoning-effort` CLI flag) controls the reasoning budget for
o-series-style models — OpenAI's o1/o3, Azure AI Foundry, Groq,
Together, and any other provider that proxies them. Allowed
values are `low`, `medium`, `high`. Local non-reasoning models and
non-supporting providers ignore the field on the wire.

## Customizing the prompts

The system prompt lives in
[`internal/prompts/review_system.md`](internal/prompts/review_system.md)
and is embedded into the binary at build time via `//go:embed`.
Golden-file tests in
[`internal/prompts/review_test.go`](internal/prompts/review_test.go)
pin the contract (read-only tool surface, JSON schema, severity
levels, categories, CI artifact references, policy awareness).
Changes go through PR review.

The user prompt is built dynamically by
`prompts.ReviewUserPrompt(meta, chunks, artifactSet)` from the
MR metadata, the diff chunks, and the loaded CI artifact set.
The function is pure — no I/O, no logging — so it can be
unit-tested with fixture inputs.

What the agent sees in the user prompt:

1. MR metadata (IID, title, author, source → target branch).
2. MR description (when present).
3. CI artifact block (build / test / lint / vulns status +
   content; explicit `NOT AVAILABLE` markers for missing files).
4. The diff itself, in labelled chunks:
   ```
   === File: internal/foo.go ===
   diff --git a/internal/foo.go b/internal/foo.go
   ...
   === End File: internal/foo.go ===
   ```
5. The closing instruction: "emit the JSON object described in
   your instructions."

**Policy-driven suffix.** The orchestrator threads the
loaded `policy.yaml` into the orchestrator's `applyPolicy` call
on the agent's findings — the prompt itself doesn't include
the policy (the harness library handles policy enforcement
transparently; the verdict is attached to findings before the
GitLab post).

**What you CAN'T do with the prompt:** redefine the JSON
schema, change the severity values, ask for non-JSON output,
or rename the response fields. Doing any of those would break
the parser. The system prompt's contract is locked in by tests.

**Skills are a softer customization path.** Skills are
team-authored review guidance (`.md` files) the agent reads
on demand via two MCP tools — without touching the prompt
contract at all. See [Skills](#skills) for the authoring
rules and override semantics; five bundled skills ship with
every release and a central repo can augment them.

## Flag reference

| Flag                          | Env var                       | Default                          |
|-------------------------------|-------------------------------|----------------------------------|
| `--gitlab-url`                | `GITLAB_URL`                  | `https://gitlab.com`             |
| `--gitlab-token`              | `GITLAB_TOKEN`                | (required)                       |
| `--provider`                  | `MREVIEW_PROVIDER`            | `local`                          |
| `--provider-base-url`         | `MREVIEW_PROVIDER_BASE_URL`   | empty                            |
| `--model`                     | `MREVIEW_MODEL`               | `qwen2.5-coder:7b`               |
| `--workdir`                   | `MREVIEW_WORKDIR`             | empty (defaults to repo root)     |
| `--tokensave-enabled`         | `MREVIEW_TOKENSAVE_ENABLED`   | `true`                           |
| `--tokensave-bin`             | `MREVIEW_TOKENSAVE_BIN`       | `tokensave` (`PATH` lookup; the Docker image has it at `/usr/local/bin/tokensave`) |
| `--artifacts-dir`             | `MREVIEW_ARTIFACTS_DIR`       | `.mreview-artifacts`             |
| `--policy-file`               | `MREVIEW_POLICY_FILE`         | empty (no policy)                |
| `--on-drafts`                 | `MREVIEW_ON_DRAFTS`           | `skip`                           |
| `--on-push`                   | `MREVIEW_ON_PUSH`             | `skip`                           |
| `--bot-username`              | `GITLAB_BOT_USERNAME`         | empty (all comments count)       |
| `--retries`                   | `MREVIEW_RETRIES`             | `3`                              |
| `--retry-backoff`             | `MREVIEW_RETRY_BACKOFF`       | `500ms`                          |
| `--dry-run`                   | (no env)                      | `false`                          |
| `--log-format`                | (no env)                      | `text`                           |
| `--verbose`                   | (no env)                      | `false`                          |
| `--config`                    | `MREVIEW_CONFIG`              | `~/.config/mreview/config.yaml`  |

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Central CI pipeline (one stage per producer + one for mreview)  │
│                                                                  │
│   go-build      ──┐                                             │
│   go-test       ──┤                                             │
│   golangci-lint  ─┼─→ .mreview-artifacts/                       │
│   govulncheck   ──┤   build.log                                  │
│   tokensave sync ┘   test_results.json                          │
│                     lint.json                                   │
│                     vulns.json                                   │
│                                                                  │
│                                  ┌─────────────────────────┐    │
│   mreview  ←────────────────────┤  Harness Runtime         │    │
│   (orchestrator + reviewer)      │                         │    │
│                                  │  Provider:              │    │
│   1. Read CI artifacts  ────────► │   Anthropic / OpenAI /  │    │
│                                  │   Gemini / LiteLLM /    │    │
│   2. Read policy.yaml ──────────► │   OpenRouter / Local    │    │
│                                  │                         │    │
│   3. Fetch MR + diff from  ────► │  Tools:                  │    │
│      GitLab (gitlab client)      │   read_file              │    │
│                                  │   mcp__tokensave__*      │    │
│   4. Build user prompt:          │     smart_context        │    │
│      MR metadata + diff          │     semantic_search      │    │
│      chunks + artifact           │     impact_analysis      │    │
│      block + policy hints        │                         │    │
│                                  │  Loop:                   │    │
│   5. Run harness ────────────────► │   think → tool call →   │    │
│                                  │   think → ... → emit    │    │
│   6. Apply policy.Enforce()  ◄───│   findings JSON         │    │
│   7. Dedupe vs existing     ◄────┘                         │    │
│      GitLab discussions                                         │
│   8. Post summary + inline  ────► GitLab API                  │
│      findings                                                  │
└──────────────────────────────────────────────────────────────────┘
```

`mreview review` is the only entrypoint. The orchestrator is a
pure Go function — no goroutines, no plugin discovery, no event
loop. Context cancellation propagates. The harness library
owns the LLM loop, streaming, prompt caching, MCP connection
lifecycle, and session persistence; mreview owns the GitLab
plumbing, the policy enforcer, the artifact loader, and the
orchestrator glue.

### Package layout

```
cmd/mreview/             CLI wiring (kong), exit codes, slog setup,
                          subcommand dispatch. The review subcommand
                          wires the orchestrator + harness runtime.
internal/event/          Per-event guard (issue #41):
                          event.Decide() + the Source/Prefer types.
                          Pure functions, table-driven tests over
                          every CI source.
internal/policy/         policy.yaml enforcement (issue #42 step 2):
                          severity_overrides / forbid / require /
                          labels. Strict validation + Enforce().
internal/diff/           Diff chunker (preserved from the old
                          internal/llm/). Language-agnostic; takes a
                          tiny Source interface so ChangeFile
                          satisfies it via a small adapter.
internal/ci/artifact/     CI artifact loaders (issue #43):
                          build.log / test_results.json / lint.json /
                          vulns.json. Per-artifact failures live in
                          LoadResult (NOT errors); LoadAll itself
                          errors only when the directory is missing.
internal/prompts/        Review prompts — system (//go:embed in
                          review_system.md) + user (pure renderer).
                          Artifact block rendering lives here too.
internal/tokensave/      tokensave MCP server config — wired into
                          AgentSpec.MCPServers.
internal/provider/       Six-provider switch (anthropic / openai /
                          gemini / litellm / openrouter / local).
                          Harness library does the wire work.
internal/gitlab/          Typed wrapper around client-go: FetchMR,
                          FetchChanges, PostSummary, PostDiscussion,
                          ListDiscussions, CurrentUser + retry /
                          backoff / typed errors.
internal/reviewer/        Orchestrator: fetch → chunk → harness →
                          parse → dedupe → post. Runner interface
                          (HarnessRunner in production, FakeRunner
                          in tests).
internal/logging/         slog JSON/text setup
internal/config/          YAML config loader (--config file flag)
internal/strutil/         Tiny string-trim helper shared across
                          packages.

examples/                 config.yaml + docker-compose.yml +
                          gitlab-ci.yml (single-repo pattern) +
                          policy.yaml + central-ci.yml (org-wide
                          pattern).
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
| Harness agent loop (one invocation per MR — the harness manages its own internal turns) | 5 – 60 s |
| Harness agent loop (CPU-only Ollama, 7B Q4) | 30 – 300 s |
| Posting comments (1 summary + N inline findings) | < 1 s total |

For a typical 5-file MR on GPU-accelerated Ollama, end-to-end
review time is **30 – 90 seconds**. On a CPU-only Raspberry Pi 5
with a 7B model, expect **5 – 15 minutes**.

### Memory

**mreview binary:** ~30 MB static, ~80 MB RSS at runtime (the
harness library brings in streaming + session + MCP
dependencies). A review consumes ~20 MB transient (mostly the
diff in memory during chunking + the rendered prompt in
memory while the harness runs).

**LLM server (Ollama example):**

| Model | RAM (inference) |
|---|---|
| 7B Q4 | ~5 GB |
| 13B Q4 | ~9 GB |
| 70B Q4 | ~40 GB |

KV cache grows with context length — a 32 k context window adds
1–4 GB on top of the model weights. The harness library
respects the model's `ContextWindow` from `AgentSpec`; pass
`--max-tokens` to bound output.

### GitLab API rate limits

`mreview review` makes 3 + N GitLab API calls per run:

- `GET /merge_requests/:iid` (1)
- `GET /merge_requests/:iid/changes` (1)
- `GET /merge_requests/:iid/discussions` (1, dedupe baseline)
- `POST /merge_requests/:iid/notes` (0 or 1, summary)
- `POST /merge_requests/:iid/discussions` (1 per inline finding)

GitLab.com's per-user rate limit is generous for `api`-scoped
PATs (2 000 requests/hour). A typical mreview run consumes
~10 requests. Self-hosted GitLab has no enforced rate limit.

`mreview` does its own retry on transient 5xx / 429 via
`internal/gitlab/retry.go`. The `Retry-After` HTTP header is
honored up to a 60-second cap (longer values get clipped).

### When to upgrade

| Symptom | Fix |
|---|---|
| Reviews take minutes per MR | Smaller model (7B → 3B); faster hardware; lower `--max-diff-bytes` |
| Harness agent loops too long | Lower `MaxTurns` in the AgentSpec (currently 10); shorten the system prompt |
| Bot posts near-duplicate findings across pushes | Dedupe is already on `(file, line, body)`; tune the bot-username filter (`--bot-username`) |
| LLM OOMs on a chunk | Lower `--max-tokens`; use a smaller context window |

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

### Skills: list_skills returns useless headings instead of descriptions

Symptom: `mcp__skills__list_skills` output for the bundled skills
looks like:

```
go-review         → "# Go code review checklist"
testing-patterns  → "# Go test patterns"
error-handling    → "# Error handling conventions"
...
```

The agent can't tell what the skills cover without `read_skill`-ing
each one.

Cause: the skill's `.md` file is missing a `description:` field in
its YAML frontmatter. The loader reads `description:` from
frontmatter verbatim (issue #44); without one, it falls back to
the first paragraph of the body — which, in this case, is the
`# H1` heading.

Fix: add a frontmatter `description:` to each affected file:

```markdown
---
title: My skill
description: One-line summary that appears in list_skills
---

# My skill

Real content starts here.
```

See [`internal/skills/bundled/skill-authoring.md`](internal/skills/bundled/skill-authoring.md)
for the full authoring rules and
[`examples/skills-repo/`](examples/skills-repo/) for a reference layout.

### Skills: my override doesn't replace the bundled skill

Symptom: the agent sees the bundled skill's body even though a
file with the same name exists in the central skills repo.

Cause: the override semantic is **filename match**, not body
match. The central repo's `skills/error-handling.md` only overrides
`bundled://error-handling.md` if the filename (minus `.md`) is
identical.

Common mistakes:

- **`error-handling.md` vs `error_handling.md`** — kebab-case is
  the convention; underscores don't match.
- **`errorHandling.md`** — camelCase doesn't match; rename to
  kebab-case.
- **Wrong directory** — the file lives under `skills/`, not at
  the repo root. mreview's `--skills-dir` defaults to `skills`;
  if your repo uses `guidelines/`, set `--skills-dir=guidelines`.

Verify with `gh api` or `curl` against the GitLab tree API; the
file must appear as a `blob` of type `markdown` under the
configured `--skills-dir`.

### Skills: 4xx / 5xx from GitLab on every list_skills call

Symptom: subprocess logs show `skills: remote fetch failed;
using bundled only` followed by a `WARN` line carrying the
HTTP status.

Cause: the central repo's GitLab instance rejected the request.

Common fixes:

- **401 / 403**: `--skills-token-env` (or the env var it points
  to) holds an expired or under-scoped token. The default is
  `--gitlab-token-env`, which is `GITLAB_TOKEN` — re-issue the
  PAT with `api` scope and verify the env var name matches.
- **404**: `repo_path` (`group/project`) is wrong, or the project
  doesn't have the `skills/` directory. Check
  `--skills-dir` against the repo's actual layout.
- **502 / 503**: GitLab is down or behind a VPN. The agent still
  sees the bundled set — graceful degradation is the headline
  contract. Re-run when the network is back.

## Examples

The `examples/` directory ships with copy-pasteable starting
points. Most operators only need one or two.

| File | When you'd use it |
|---|---|
| [`config.yaml`](examples/config.yaml) | Persist flags + env across invocations — every secret references an env-var NAME (never a value). Drop at `~/.config/mreview/config.yaml` or pass with `--config`. |
| [`docker-compose.yml`](examples/docker-compose.yml) | Local dev or single-host production: brings up Ollama + mreview with health checks and a named volume for the model. |
| [`gitlab-ci.yml`](examples/gitlab-ci.yml) | Single-repo CI-driven review — runs on every MR pipeline. Best for small teams that want the simplest possible setup. |
| [`central-ci.yml`](examples/central-ci.yml) | **Reference template** for org-wide onboarding — producer stages (build / test / lint / vulns / tokensave sync) + mreview stage. Copy into your org's CI templates repo. |
| [`policy.yaml`](examples/policy.yaml) | Reference policy.yaml file (severity_overrides, forbid, require, labels). Drop at the path passed via `--policy-file`. |

A typical CI-driven setup uses **two**: copy `central-ci.yml`
into your org's CI library repo, and `policy.yaml` into the
repo that wants policy enforcement. Teams that run mreview
locally for development add `docker-compose.yml` +
`config.yaml`. The team-prompt customization knobs from the
pre-reset architecture are no longer exposed — the system
prompt is owned by the embedded
[`internal/prompts/review_system.md`](internal/prompts/review_system.md)
and pinned by golden-file tests.

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

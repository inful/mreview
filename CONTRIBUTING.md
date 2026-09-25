# Contributing

mreview is licensed under
[AGPL-3.0-or-later](https://www.gnu.org/licenses/agpl-3.0.html).
By submitting a contribution, you agree to license it under the
same terms.

## Workflow

1. Fork the repository.
2. Install the pre-commit hook: `lefthook install`.
3. Make your change on a topic branch.
4. Write tests **first** — every code change follows TDD:
   red (failing test) → green (impl) → refactor → tests green
   → lint clean → conventional commit.
5. Open a pull request against `main`.

## Local commands

```bash
go test -race -coverprofile=coverage.out ./...   # full test suite (race)
go vet ./...                                    # static checks
golangci-lint run                               # full lint (matches CI)
go build -trimpath -ldflags="-s -w" -o dist/mreview ./cmd/mreview
go mod tidy                                     # tidy go.mod / go.sum
```

The lefthook pre-commit runs `golangci-lint run` and
`go test -race -count=1 ./...` on staged changes.

## Commit messages — Conventional Commits

```
feat: add per-chunk retry on transient LLM errors
fix: handle 401 from gitlab.com in dedupe
test: cover oversize-file rejection path
docs: clarify --webhook-secret usage
refactor: extract fingerprint normalization
chore: bump go-gitlab to v1.47.0
```

The release pipeline (GoReleaser) parses these to build the
CHANGELOG and decide version bumps.

- `feat:` → minor bump (0.1.0 → 0.2.0)
- `fix:` → patch bump (0.1.0 → 0.1.1)
- `feat!:` / `BREAKING CHANGE:` footer → major bump (0.1.0 → 1.0.0)

## Adding a new LLM provider

After the architecture reset ([#42](https://github.com/inful/mreview/issues/42)),
the provider matrix is owned by the
[harness](https://github.com/sausheong/harness) library, not mreview
itself. mreview only forwards three pieces to the harness:

- the **provider name** (`--provider`: `anthropic` / `openai` / `gemini` /
  `litellm` / `openrouter` / `local`)
- the **model name** (`--model`, e.g. `qwen2.5-coder:7b`)
- the **base URL** for proxy / local providers (`--provider-base-url`,
  e.g. `http://localhost:11434/v1`)

The per-provider API-key env vars (`ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`, `GOOGLE_API_KEY`, `LITELLM_API_KEY`,
`OPENROUTER_API_KEY`) are read by the harness library directly. mreview
just passes the values through. To add a new provider, add the
provider name to the enum in `cmd/mreview/review.go` and wire the
new key lookup into `providerAPIKey` in the same file.

The OpenAI-compatible backend (`--provider=local`) covers Ollama /
llama.cpp / vLLM / LM Studio via a base URL swap, so most
new "providers" are really new backends behind an
OpenAI-compatible shim — no mreview code changes needed for those.

## Adding a new GitLab client method

Wrap upstream's `gitlab.com/gitlab-org/api/client-go` in
`internal/gitlab/`. Every method should:

- Validate project path + IID via `validatePath` / iid range check.
- Use `doWithRetry` with a sensible `RetryConfig`.
- Map upstream errors to typed `*Error` via `classifyAndWrap`.
- Cap response bodies at 64 KiB.
- Log success with `attempt` count.

Tests live in the corresponding `*_test.go` using a `httptest`
stub that scripts response sequences.

## Project layout

```
cmd/mreview/             kong wiring, exit codes, subcommand dispatch
internal/gitlab/         Typed wrapper around client-go (incl. repository-files transport)
internal/event/          Per-event guard (issue #41): draft + push skip
internal/policy/         policy.yaml enforcement (issue #42 step 2)
internal/diff/           Diff chunker (file + hunk splitting)
internal/ci/artifact/    CI artifact loaders (issue #43)
internal/prompts/        System prompt (//go:embed) + user prompt renderer
internal/reviewer/       Orchestrator: fetch → chunk → harness → parse → dedupe → post
internal/provider/       Provider name + base URL + key forwarding to harness
internal/tokensave/      tokensave MCP server config (wired into AgentSpec)
internal/skills/         Skills loader (.md discovery + cache)
  internal/skills/mcp/   MCP server exposing list_skills / read_skill over stdio
  internal/skills/bundled/  .md files embedded into every binary (5 default skills)
internal/logging/        slog setup
internal/config/         YAML config loader (--config flag)
internal/strutil/         Tiny string-trim helper
examples/                config.yaml, docker-compose.yml, gitlab-ci.yml, central-ci.yml, policy.yaml, skills-repo/
```

See `README.md` for the operator-facing docs.

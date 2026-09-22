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

The reviewer depends on `llm.Provider` (interface in
`internal/llm/provider.go`). To add a backend:

1. Write a new type implementing `Chat(ctx, ChatRequest) (*ChatResponse, error)`.
2. Add it to `cmd/mreview/review.go` and `cmd/mreview/serve.go` constructor paths.
3. Add the provider type to the docstrings in those files.
4. Cover with stub-server tests under `internal/llm/`.

The OpenAI-compatible backend (`internal/llm/openai.go`) already
covers Ollama / llama.cpp / vLLM / LM Studio via a base URL swap,
so most new "providers" are really new backends behind an
OpenAI-compatible shim.

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
internal/gitlab/         Typed wrapper around client-go
internal/llm/            Provider + parser + chunker + prompt
internal/reviewer/       Orchestrator (ReviewMR)
internal/server/         Webhook HTTP receiver + worker pool
internal/logging/        slog setup
examples/                config.yaml, docker-compose.yml, gitlab-ci.yml, webhook-setup.md
```

See `README.md` for the operator-facing docs.

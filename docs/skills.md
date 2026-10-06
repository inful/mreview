---
aliases:
  - /_uid/skills/
  - /_uid/205f26e7-35b5-4ba8-816b-6738cd5d6d1f/
categories:
  - explanation
date: 2026-09-27T00:00:00Z
fingerprint: c80a284caedc24a6049f990c67c020577ff8e5b664a005bf9eb6283c990e6ee7
lastmod: "2026-09-27"
tags:
  - skills
  - prompts
  - customization
  - mcp
title: Skills
uid: 205f26e7-35b5-4ba8-816b-6738cd5d6d1f
weight: 50
---

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

## Two layers: bundled + central

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

## Override semantics: central wins

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

## Authoring skills for the central repo

One skill is one `.md` file in a shared GitLab repository's
`skills/` directory:

```markdown
---
title: Go review checklist
description: One-sentence summary of what this skill covers
---

# Go review checklist

- Flag any use of `panic` outside `cmd/.../main.go`.
- ...
```

The `description:` field is what surfaces in `mcp__skills__list_skills`
(capped at 200 chars). When frontmatter has no `description:`,
the loader falls back to the first non-blank paragraph of the
body — so a `# H1` as that paragraph would surface as `# H1` in
`list_skills` (not informative). Keep the description to one or
two sentences so the agent can scan the list cheaply.

For a complete reference layout — README, four example
skills, the auth + token config — see
[`examples/skills-repo/`](https://github.com/inful/mreview/tree/main/examples/skills-repo/).

For the meta-circular review pipeline (the CI that reviews
*this* repo's skill MRs using the bundled `skill-authoring`
skill), see
[`examples/skills-gitlab-ci.yml`](https://github.com/inful/mreview/tree/main/examples/skills-gitlab-ci.yml).
Drop it in the root of your central skills repo; the agent
will review each skill MR against the layout / authoring
contract documented in `internal/skills/bundled/skill-authoring.md`.

## Configuring skills

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

---

[← Back to index](index.md) · [Next: What the bot posts →](what-the-bot-posts.md)
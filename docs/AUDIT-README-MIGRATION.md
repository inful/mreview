---
aliases:
  - /_uid/audit-readme-migration/
  - /_uid/audit-2026-10-06/
categories:
  - meta
date: 2026-10-06T00:00:00Z
lastmod: "2026-10-06"
tags:
  - audit
  - docs
title: README → docs/ migration audit
uid: a0b0c0d0-e0f0-2026-1006-audit-readme
weight: 999
---

## Why this exists

A pre-v0.9.1 refactor collapsed the operator-facing
README into a short pointer file and moved the canonical
content into the `docs/` directory. The split is
intentional (the README stays GitHub-friendly; docs/ is the
long-form source of truth) but it carries risk: a section
removed from README might not have a clean home in
docs/, leaving an operator looking for that information
with no breadcrumbs.

This audit enumerates every `## ` heading in the pre-refactor
README and confirms each has a 1:1 home in the post-refactor
docs/ tree. The audit was triggered by a code review of the
mreview repo on 2026-10-06.

## Mapping table

| Pre-refactor README section | Post-refactor home | Match quality |
|---|---|---|
| Why mreview | `docs/index.md` "Why mreview?" | exact |
| How it works | `docs/how-it-works.md` | exact |
| Install | `docs/getting-started.md` "Install" | exact |
| Quickstart | `docs/getting-started.md` "Quickstart" | exact |
| Subcommands | `docs/subcommands.md` | exact |
| Behaviour by event | `docs/behaviour-by-event.md` | exact |
| Policy enforcement | `docs/policy-enforcement.md` | exact |
| What the bot posts | `docs/what-the-bot-posts.md` | exact |
| mreview summary (sub-section) | `docs/what-the-bot-posts.md` "mreview summary" | exact (now a sub-heading) |
| Exit codes | `docs/configuration.md` "Exit codes" | exact |
| Read-only tool surface | `docs/how-it-works.md` "Read-only tool surface" | exact |
| Tokensave bundle | `docs/onboarding-central-ci.md` "Tokensave bundle" | exact |
| CI artifact reuse | `docs/configuration.md` "CI artifact reuse" | exact |
| Skills | `docs/skills.md` | exact |
| Configuration | `docs/configuration.md` | exact |
| Onboarding via central CI | `docs/onboarding-central-ci.md` | exact |
| Customizing the prompts | `docs/configuration.md` "Customizing the prompts" | exact |
| Flag reference | `docs/subcommands.md` "Flag reference" | exact |
| Architecture | `docs/how-it-works.md` "Architecture" | exact |
| Performance & cost | `docs/performance-cost.md` | exact |
| Development | `docs/development.md` | exact |
| Troubleshooting | `docs/troubleshooting.md` | exact |
| Examples (overview) | `docs/onboarding-central-ci.md` "Examples" + `examples/` directory | exact (links preserved) |
| License | `LICENSE` file at repo root | external (license file, not docs) |

**No `missing` rows.** Every section removed from the
README has a 1:1 home in docs/ (or, in the case of License,
in a file that's always been at the repo root).

## Cross-reference spot check

A grep across all docs/ files for relative `.md` links
confirms no broken internal references:

```bash
$ grep -nE '\]\(\.{1,2}/[^)]+\.md\)' docs/*.md
# (no output — no relative markdown links)
```

(Relative links were replaced with absolute URLs pointing
at the GitHub repo in the v0.5.0 → v0.9.1 audit commits
786c623 and 0398c85. This is intentional: docs are read
both from the source tree and from the GitHub web view, and
absolute URLs work in both.)

## scripts/check-commit-msg.sh

The new commit-message gate script is exercised by the
`commit-msg` lefthook hook and by the project's
`CONTRIBUTING.md` documentation. Behavioural verification
(test in the `make test` target) is a follow-up —
see `.planning/PHASE-fix-review-findings.md` sub-phase 3
task 3.4.

## How to re-run this audit

This is a one-time document. Re-run if (a) another
README refactor happens, (b) a new docs/ page is added
without a clear README home, or (c) the docs/ page for
an existing README section is renamed or removed.

The mapping table above is the source of truth. To
verify it, run:

```bash
# Pre-refactor headings
git show HEAD:README.md | grep -E '^## '

# Post-refactor headings
for f in docs/*.md; do
  echo "--- $f ---"
  grep -E '^## ' "$f"
done
```

Every README heading should appear (possibly in a
slightly different form) in the second list.

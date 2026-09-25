---
title: MR description conventions
description: How merge-request descriptions are structured in this team's GitLab
---

# MR description conventions

Every MR in this team's GitLab follows the same template. The
review agent uses these conventions when scanning diffs + reading
the MR body.

## Required sections

1. **What**: one paragraph on what changed. End with a sentence
   that answers "why this matters" — what user-visible behaviour
   does this MR enable?

2. **How**: bullet list of the moving parts. Each bullet is a
   logical change; commit-level detail belongs in the commits.

3. **Test plan**: how the author verified the change. Unit tests
   run by CI; the test plan is for *manual* verification — local
   repro steps, smoke-test commands, expected output.

4. **Risk + rollback**: what could break, and how to roll back
   (the previous tag, the revert commit, the feature flag to
   disable).

## Labels

- `bug`, `feature`, `refactor`, `docs`, `chore` — always.
- One area label (`area/api`, `area/cli`, etc.) when applicable.
- `breaking-change` when the MR introduces a backwards-
  incompatible change. The policy layer escalates findings on
  files in such MRs to `error` severity automatically.

## Linked issues

Every MR closes at least one issue via `Closes #N`. The MR
description links the issue in the first paragraph. Reviewers
verify the linked issue's acceptance criteria match what the
MR delivers.

## Title format

`<area>: <verb> <object>` — e.g. `cli: add --skills-repo flag`,
`api: paginate user list`. Area is the same label vocabulary
above. Verb is past tense (`add`, `fix`, `refactor`).

---
aliases:
  - /_uid/what-the-bot-posts/
  - /_uid/b6532c8d-28d9-4551-9474-c4a77d51a143/
categories:
  - reference
date: 2026-09-27T00:00:00Z
fingerprint: 32da54f2c39f4e3cd20223de540814df3d9a2aa2dfc6def4040051a2fa91eb53
lastmod: "2026-09-27"
tags:
  - output
  - gitlab
  - summary
  - inline-discussion
title: What the bot posts
uid: b6532c8d-28d9-4551-9474-c4a77d51a143
weight: 60
---

A successful review produces **one summary note** + **zero or more
inline discussion threads** anchored to specific lines. The
shapes below come from `internal/reviewer/summary.go` and the
GitLab discussion API; you'll see them in the MR timeline as
soon as the first review completes.

## Summary note

A single top-level note (not anchored to any line) per MR.
**The bot edits the same note in place on every run** — the
MR's activity feed shows one mreview note that evolves over
time, not a stack of stale summaries. The first review on a
fresh MR posts a fresh note; every subsequent run (including
push events) edits that same note in place. PUT (`EditSummary`)
doesn't bump the activity feed the way POST does, so the
"stale summary on every push" spam is gone.

The summary body carries a hidden `<!-- mreview:commit=… -->`
marker at the top. The next mreview run on the same MR reads
this marker to (1) skip if the commit SHA matches, or (2)
extract the prior findings for the LLM to evaluate when the
SHA differs. The marker is invisible in GitLab's rendered
view.

```markdown
## mreview summary

LGTM with 2 nitpicks — secret caching and missing test for the
new eviction hook.

---
<details>
<summary>Findings (3)</summary>

| Status | Severity | File:Line | Category | Comment |
|--------|----------|-----------|----------|---------|
| 🆕 new | 🛑 error | `internal/auth/jwt.go:142` | security | JWT secret should be cached |
| ↻ carried over | (prior) | `internal/cache/cache.go:55` | (prior) | Missing test for new eviction hook |
| ✓ resolved | (prior) | `internal/cache/cache.go:80` | (prior) | Original: bug in eviction order<br><em>Resolved: eviction was rewritten in this commit</em> |

</details>
```

The `<details>` block keeps the MR timeline clean — the table is
collapsed by default; readers expand it to see the per-finding
breakdown.

### Status column reference

The **Status** column is new as of the prior-finding-close
feature. Every row in the findings table has exactly one
status; the legend is:

| Glyph | Label            | Meaning                                                            |
|-------|------------------|--------------------------------------------------------------------|
| 🆕    | new              | Surfaced for the first time on this run.                           |
| ↻     | carried over     | Present in a prior run; still valid in the new diff.               |
| ✓     | resolved         | Present in a prior run; the LLM judged the new diff addressed it.  |
| ∅     | out of scope     | Present in a prior run; no longer relevant (e.g. file deleted).   |

A row whose status is `resolved` or `out of scope` has its
prior GitLab discussion auto-resolved. The row body includes
the LLM's *rationale* in italics so the operator can see
*why* the auto-resolve happened without clicking through to
the (now-collapsed) thread. A row whose status is
`carried over` shows `(prior)` in the Severity and Category
columns — the original is one click away on the still-open
discussion.

## Inline discussion

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

## When the bot stays silent

The bot posts **nothing** when:

- The diff is empty (trivial MR, e.g. comment-only changes) — the
  filter still runs but no chunks survive.
- Every finding is deduped against the bot's prior comments — no
  new comments to post. The summary is still edited in place (the
  prior findings the LLM marked as `still_valid` stay visible in
  the table as "carried over" rows; the table just doesn't grow).
- The harness agent's LLM call fails (transport error, non-zero
  exit after the retry budget is exhausted, or the configured
  `MaxTurns` is reached without emitting the findings JSON).
  The review aborts with exit code 7 — no summary note, no inline
  discussion. The log carries the chunk index, file list, attempt
  count, and underlying error; operators re-run rather than
  trusting a half-completed report.

## Explicit non-silent paths

Operators running with `--dry-run` see every post *attempted* in
the logs (with body and URL) without anything actually landing on
GitLab — use this to verify what the bot will say before letting it
hit a real MR.

---

[← Back to index](index.md) · [Next: Configuration →](configuration.md)
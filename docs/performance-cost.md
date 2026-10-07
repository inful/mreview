---
aliases:
  - /_uid/performance-cost/
  - /_uid/34449c55-45fc-4fab-9f9d-eec978fe1460/
categories:
  - reference
date: 2026-09-27T00:00:00Z
fingerprint: 3e854965b3b8b05fdc860e37785a13c0a983b5a696df8e7393ff05154a6dc69c
lastmod: "2026-09-27"
tags:
  - performance
  - cost
  - tokens
  - latency
  - memory
title: Performance and cost
uid: 34449c55-45fc-4fab-9f9d-eec978fe1460
weight: 100
---

Numbers below are typical — actuals depend on the model, the
hardware it runs on, and the shape of your MRs.

## Token usage

The prompt uses a chars/4 heuristic to estimate tokens (no
per-model tokenizer — keep the binary small). Rough per-review
budget for a **500-line MR across 5 files** (one chunk per file):

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

## Latency

| Stage | Typical time |
|---|---|
| GitLab API calls (3 calls per review: fetch MR, fetch changes, list discussions) | < 1 s total |
| Harness agent loop (one invocation per MR — the harness manages its own internal turns) | 5 – 60 s |
| Harness agent loop (CPU-only Ollama, 7B Q4) | 30 – 300 s |
| Posting comments (1 summary + N inline findings) | < 1 s total |

For a typical 5-file MR on GPU-accelerated Ollama, end-to-end
review time is **30 – 90 seconds**. On a CPU-only Raspberry Pi 5
with a 7B model, expect **5 – 15 minutes**.

## Memory

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

## GitLab API rate limits

`mreview review` makes 3 + N GitLab API calls per run:

- `GET /merge_requests/:iid` (1)
- `GET /merge_requests/:iid/changes` (1)
- `GET /merge_requests/:iid/discussions` (1, dedupe baseline)
- `POST /merge_requests/:iid/notes` (0 or 1, summary — first run only)
- `PUT /merge_requests/:iid/notes/:note_id` (0 or 1, summary — every subsequent run; edits the existing note in place)
- `POST /merge_requests/:iid/discussions` (1 per inline finding)
- `PUT /merge_requests/:iid/discussions/:discussion_id/resolve` (1 per prior finding the LLM marked `resolved` or `out_of_scope`)

GitLab.com's per-user rate limit is generous for `api`-scoped
PATs (2 000 requests/hour). A typical mreview run consumes
~10 requests. Self-hosted GitLab has no enforced rate limit.

`mreview` does its own retry on transient 5xx / 429 via
`internal/gitlab/retry.go`. The `Retry-After` HTTP header is
honored up to a 60-second cap (longer values get clipped).

## When to upgrade

| Symptom | Fix |
|---|---|
| Reviews take minutes per MR | Smaller model (7B → 3B); faster hardware |
| Harness agent loops too long | Lower `--max-turns` (default 6); shorten the system prompt |
| Bot posts near-duplicate findings across pushes | Dedupe is already on `file:line`; tune the bot-username filter (`--bot-username`); pass `--no-dedup` to force a re-review |
| LLM OOMs on a chunk | Lower `--max-output-tokens` (default 16384); use a smaller context window |

---

[← Back to index](index.md) · [Next: Troubleshooting →](troubleshooting.md)
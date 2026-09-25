---
title: tokensave tool selection
description: When to use which mcp__tokensave__* tool during a review
---

# tokensave tool selection

mreview's review agent reaches the reviewed code through the
tokensave MCP server. Choosing the right tool saves time and
tokens.

## Decision order

1. **`mcp__tokensave__smart_context`** — your first call for any file. Asks "what does this code do / what depends on it" and returns a textual answer plus referenced symbols. Use for **understanding** unfamiliar code.
2. **`mcp__tokensave__read`** — raw byte access. Use mode `lines` with a `A-B` range for targeted reads, `full` for whole files, `map` for a non-byte symbol index. Use for **verbatim** content (configs, READMEs, error messages).
3. **`mcp__tokensave__impact_analysis`** — blast radius. "What breaks if I change this file?" Run this *before* flagging a finding about an API change, signature change, or shared utility.
4. **`mcp__tokensave__semantic_search`** — natural-language query across the repo. "Where is authentication handled?" Use for **discovery** when you don't know which file holds something.
5. **`mcp__tokensave__search` / `body` / `callers` / `callees`** — symbol-level work. Use these when you have a specific function or type name and want its definition or call graph.

## Don't

- Don't call `smart_context` for every file in the diff. One per *concept*, not per file.
- Don't call `read` with `full` for files > ~500 lines. Use `lines` with a range.
- Don't re-implement file walking with multiple `read` calls when `smart_context` would answer the question.

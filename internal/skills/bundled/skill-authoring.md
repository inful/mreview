---
title: Skill authoring
description: Layout and content rules for .md files under a skills/ directory
---

# Skill authoring

When reviewing an MR that adds or changes files under a
`skills/` directory, look for these rules. Every `.md` file
under `skills/` is consumed by mreview as a single skill —
its filename (minus `.md`) becomes the skill's handle, and
its first non-empty paragraph (after any frontmatter)
becomes the description that surfaces in
`mcp__skills__list_skills`. The rules below keep that
contract honest.

## Filename

- **Kebab-case**: `api-design.md`, not `apiDesign.md` or
  `api_design.md`. The filename is the agent's handle for
  `mcp__skills__read_skill`; lowercase-with-dashes reads
  naturally in tool calls.
- **`.md` extension only**. The loader silently skips other
  extensions; an `.mdx` or `.markdown` file is invisible
  to the agent. Flag non-`.md` files in `skills/` as a
  warning.
- **One skill per file**. Don't bundle multiple skills in
  one `.md` — the loader keys on filename, not section
  headings.

## Frontmatter

YAML frontmatter between two `---` markers is optional but
encouraged. The loader skips it before extracting the
description, so frontmatter content doesn't affect
`list_skills` output. Recommended keys:

```yaml
---
title: API design guidelines
description: Team conventions for HTTP and RPC API design
---
```

- `title:` — human-readable name for editor previews. Not
  consumed by the agent.
- `description:` — what *you* would put in `list_skills` if
  the body didn't have a clear first paragraph. The loader
  prefers the first-paragraph version.

## Description (first non-empty paragraph)

The description is the first non-blank line of the body
after the frontmatter block, capped at 200 characters.
Follow these rules:

- **Self-contained**. Don't start with "This skill..." or
  "Use this for..." — those prefixes waste the cap.
- **One or two sentences** describing what the skill
  covers.
- **No headings as descriptions**. A `# H1` line counts as
  the first paragraph and surfaces as `# H1` in
  `list_skills` — not informative. Put the prose
  description **before** the heading.

## Body

- **Markdown only** — no HTML, no embedded scripts.
- **Aim for one screen**: 10–30 lines is the sweet spot.
  Longer bodies eat the agent's tool-result budget; the
  agent has a finite window per `read_skill` call.
- **No executable code**. Skills are advisory context, not
  runnable. Code blocks should illustrate a pattern, not
  be a test fixture or example command for the agent to
  invoke.
- **Examples beat prose**. A "good / bad" snippet reads
  faster than three paragraphs of explanation.

## Override semantics

When authoring a skill that **replaces a bundled one**
(same filename as a skill mreview ships with), the file is
a complete replacement — the bundled version is hidden
from the agent. Make the override intent obvious in the
first paragraph:

```markdown
# API design guidelines (team override)

This file overrides the bundled `api-design.md` with our
team's stricter conventions on URL paths and status codes.
```

The agent sees only your version; the bundled default is
silently discarded. If you want to *supplement* the
bundled skill with team-specific additions, give the new
file a **distinct name** (`api-design-strict.md`) so both
are visible in `list_skills`.

## Repository README

The skills repo itself should have a top-level `README.md`
that explains:

1. How to author a skill (this file's rules, summarised).
2. How to point mreview at the repo
   (`--skills-repo=group/project`).
3. The relationship to the bundled set: bundled skills can
   be overridden by name; team-specific skills get distinct
   names.

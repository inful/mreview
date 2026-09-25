# Example central skills repo for mreview

This directory is a **reference layout** — copy it, push it to a
GitLab project, point mreview at it with `--skills-repo=group/project`,
and the agent picks up every `.md` file under `skills/`.

## Layout

```
my-skills-repo/
├── README.md           # this file (rename / replace for your team)
└── skills/
    ├── go-review.md          # mirrors the bundled skill — drop or override
    ├── testing-patterns.md   # mirrors the bundled skill — drop or override
    ├── error-handling.md     # mirrors the bundled skill — drop or override
    ├── api-design.md         # team-specific, never bundled
    └── security-checklist.md # team-specific, never bundled
```

## Layout is auto-checked on review

When mreview reviews an MR against this repo, the agent reads
the bundled `skill-authoring` skill first (the meta-circular
check). It then applies those rules to the diff:

- Kebab-case filenames, `.md` extension only
- Frontmatter between `---` markers, optional but encouraged
- First non-empty paragraph is the `list_skills` description
  (200-char cap); no `# H1` as the description
- One skill per file; aim for 10–30 lines of body
- Override intent must be clear when replacing a bundled skill

You don't need to memorise these — the reviewer will flag
violations by name. The bundled skill also documents what
counts as a violation so the agent's findings stay
consistent run-to-run.

## How mreview loads this

- **Default `directory`**: `skills` (configurable via `--skills-dir`).
- **Default `ref`**: `main` (configurable via `--skills-ref`).
- **Filter**: only files with `.md` extension become skills. Trees
  and other files are silently skipped.
- **Description extraction**: the first non-empty paragraph after a
  YAML frontmatter block, capped at 200 characters.
- **Override semantics**: a `.md` here with the same name as a
  bundled skill **wins** (e.g. drop your team's stricter
  `error-handling.md` here and the bundled default is hidden).

## Authoring rules

1. **One skill = one file**. Filename (minus `.md`) is the skill's
   handle for `mcp__skills__read_skill`. Use kebab-case names so they
   read naturally in tool calls.

2. **YAML frontmatter** is optional but encouraged — the loader
   reads `description:` from frontmatter to populate
   `list_skills`:

   ```markdown
   ---
   title: API design guidelines
   description: One-sentence summary that appears in list_skills output
   ---

   # API design guidelines

   - Use kebab-case in URL paths.
   - ...
   ```

   The description is what the agent sees in `list_skills` —
   keep it to one or two sentences so scanning is cheap.
   Without a frontmatter description, the loader falls back to
   the first paragraph of the body (and `# H1` as that
   paragraph would surface as `# H1` in `list_skills` — not
   informative).

3. **Body length**: there is no hard cap. Bodies are passed to the
   agent verbatim via `mcp__skills__read_skill`. Aim for one screen
   of markdown; longer skills risk pushing the agent off its tool-
   result budget.

4. **No code blocks that depend on running**: skills are advisory
   context for the review agent, not runnable code.

## Examples

See the four files in `skills/` for the format:

- `go-review.md` — mirrors the bundled default (override example);
  replace the body to tighten the bundled rule.
- `api-design.md` — team-specific HTTP/RPC conventions; never bundled.
- `security-checklist.md` — team-specific auth / secrets / crypto;
  never bundled.
- `mr-description.md` — team's MR template + labels; never bundled.

## Pointing mreview at this repo

```yaml
# ~/.config/mreview/config.yaml
gitlab:
  url: https://gitlab.example.com
  token_env: GITLAB_TOKEN
skills:
  repo_path: group/mreview-skills    # group/project path
  directory: skills                  # default
  ref: main                          # default; pin a SHA for reproducibility
```

```bash
# or via CLI flags
mreview review --repo=foo/bar --mr=42 \
  --gitlab-token=$TOKEN \
  --skills-repo=group/mreview-skills
```

## Related

- mreview issue [#44](https://github.com/inful/mreview/issues/44) —
  the original design discussion.
- mreview's bundled skills: `internal/skills/bundled/*.md` in the
  mreview source tree.

package artifact

// LintIssueCap caps the number of issues the lint loader
// keeps in the LintResult.Issues slice. A repo with 10k lint
// findings shouldn't bloat the prompt — the agent sees the
// count and a sample, and can call tokensave for more.
const LintIssueCap = 100

---
title: Security review checklist
description: What to flag when reviewing changes that touch auth, secrets, or external I/O
---

# Security review checklist

This skill is non-exhaustive — pair it with the bundled
`go-review` skill and any team-specific threat model. When
reviewing a change that touches any of the areas below, apply
the matching checklist.

## Authentication

- **Token handling**: secrets must come from env vars, never
  hardcoded. The `token_env:` config pattern is the canonical way;
  check for `GITLAB_TOKEN = "..."` literals and flag them as
  errors.
- **Token scope**: a Personal Access Token should have the minimum
  scope required. If a change introduces a token with `api` scope
  where `read_api` would do, that's a finding.
- **Token logging**: token values must never appear in logs. Flag
  `logger.Info("authenticated as", token)` and similar.
- **Session expiry**: long-lived sessions are a smell. Use short-
  lived tokens + refresh, or OAuth flows with proper expiry.

## Authorization

- **Check the actor**: every privileged action should verify the
  caller has the right permission. Look for handler functions that
  mutate state without consulting a policy / role check.
- **Default-deny**: when in doubt, deny. If the policy code returns
  `nil` (no opinion), the action should be rejected, not allowed.

## Input handling

- **SQL**: parameterised queries only. Look for `fmt.Sprintf`
  building SQL strings — those are errors, not warnings.
- **Shell**: any user input flowing into `exec.Command` arguments
  is a finding. The bundled mreview contract forbids shell access
  for the agent; the same rule applies to operator-built tools.
- **Path traversal**: file-serving handlers should reject `..` in
  user-supplied paths. Use `filepath.Clean` + a check that the
  result is still under the configured root.

## Secrets

- **No secrets in code**: scan the diff for any string that looks
  like a key, token, or password. Common shapes: `sk_live_...`,
  `ghp_...`, `glpat-...`, `-----BEGIN ... PRIVATE KEY-----`.
- **No secrets in config files committed to git**: the YAML config
  references env-var NAMES, never values. Flag any direct value.
- **Logging redaction**: high-cardinality identifiers (UUIDs, hash
  digests) often correlate with secrets. Log at DEBUG, never INFO.

## Cryptography

- **Hashing**: use `crypto/sha256` (or stronger). `md5` and `sha1`
  are not collision-resistant for security purposes.
- **Random numbers**: `math/rand` for any security-sensitive
  context is a finding. Use `crypto/rand`.
- **TLS**: minimum version 1.2. Verify with `crypto/tls` config;
  flag `MinVersion: tls.VersionTLS10` or `tls.VersionTLS11`.

## Dependencies

- **New deps**: any new dependency should be checked against
  `govulncheck`. If the CI artifact `vulns.json` shows known
  vulnerabilities in a dep, that's an error.
- **Replaced deps**: bumping a dep version is OK; downgrading
  without a reason is a smell.

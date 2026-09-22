# Security policy

## Reporting a vulnerability

**Please do not file a public issue for security problems.**

Report security issues privately via
[GitHub Security Advisories](https://github.com/inful/mreview/security/advisories/new)
for this repository. You'll get an acknowledgement within 72 hours
and a fix timeline once we've triaged.

If you'd rather not use GitHub, contact the maintainers via the
addresses listed in the Git commit history (`git log` →
`Author` lines).

## What to include

- A short description of the issue and its impact.
- Reproduction steps (a sample payload / curl command is great).
- The affected versions (`mreview version` output, or the
  commit SHA you built from).
- Whether you want public credit in the advisory's "Credits"
  section.

## Supported versions

| Version | Supported           |
|---------|---------------------|
| latest  | ✅                  |
| < latest | ❌ upgrade or pin  |

`mreview` is a single-binary tool with no long-lived data; the
fix path is "upgrade and redeploy." We don't backport security
patches to old releases.

## What we'll do

1. **Acknowledge** the report within 72 hours.
2. **Investigate** and confirm the issue (or close as not-applicable
   with a reason).
3. **Patch** the latest release, publish a tagged release, and
   push a new container image.
4. **Credit** the reporter in the advisory (unless they prefer
   anonymity).
5. **Disclose** publicly after the fix is widely available.

## Webhook secret handling

The `mreview serve` subcommand validates GitLab webhook
deliveries using `X-Gitlab-Token` (constant-time HMAC compare).
Treat the secret like any other credential:

- Rotate when operators with access leave the team.
- Use HTTPS in production.
- Don't log the secret; `mreview serve`'s logging never echoes
  the header value.

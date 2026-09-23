# syntax=docker/dockerfile:1.7
#
# Image for mreview, consumed by GoReleaser dockers_v2.
#
# GoReleaser pre-builds the mreview binary for every target platform
# and lays it out under <workdir>/<TARGETPLATFORM>/mreview in the
# build context. This Dockerfile does the final COPY + labels; it
# does NOT build Go code itself (see goreleaser.yaml warning "Don't
# build binaries in your Dockerfile" for why we avoid that).
#
# Multi-arch notes:
#   - $TARGETPLATFORM is the runtime platform the image is built for;
#     buildx resolves it at build time per architecture.
#   - $TARGETARCH is the per-arch identifier (amd64 / arm64) used to
#     pick the matching tokensave release asset.
#   - The base image (gcr.io/distroless/static:nonroot) is a manifest
#     list, so buildx pulls the right arch variant automatically.
#
# Runtime base:
#   - gcr.io/distroless/static:nonroot ships ca-certificates
#     (required for TLS to GitLab SaaS / self-hosted GitLab / Ollama
#     behind TLS) and nothing else. Runs as uid 65532 by default.
#
# ─── Tokensave bundle ────────────────────────────────────────────
# tokensave is the language-agnostic code-graph tool mreview's
# agent uses as its primary MCP server. Without it on PATH inside
# the image, the harness spawn fails and mreview silently falls back
# to read_file-only mode — losing the agent's primary tool.
#
# We bundle the binary in this image so the example CI templates
# work out of the box. The MCP contract pinned by tests is:
#   - CLI:    `tokensave serve --path <path>`
#   - ns:     `mcp__tokensave__*` (see internal/tokensave/mcp_test.go)
#   - tools:  smart_context / semantic_search / impact_analysis
#             (see internal/prompts/review_system.md)
#
# Version pinning: 7.12.1 (released 2026-09-12). v7.12.0 was
# withdrawn due to a broken release pipeline; do not downgrade
# without re-checking the MCP contract.
#
# Fail-closed: if the upstream SHA256SUMS asset ever disappears, the
# build fails — same policy as tokensave's own `upgrade` command.

# Stage 1: download + verify + unpack the pinned tokensave release.
# Alpine is used only for curl + sha256sum + tar; nothing from this
# stage except /out/tokensave ships in the final image.
FROM alpine:3.20 AS tokensave
ARG TARGETARCH
ARG TOKENSAVE_VERSION=7.12.1
ARG TOKENSAVE_SHA_AMD64=184612db16800e384a1bcdc7fadcc53fa73bda70240f9c0416ac4b88c7e924fb
ARG TOKENSAVE_SHA_ARM64=7de5c95b51d508f39a83d9420ad0700502de107a61dbb38dad3d008d4f6f4261

RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) SHA="${TOKENSAVE_SHA_AMD64}"; ASSET="x86_64"  ;; \
        arm64) SHA="${TOKENSAVE_SHA_ARM64}"; ASSET="aarch64" ;; \
        *) echo "Unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /out; \
    curl -fsSL --retry 3 -o /tmp/tokensave.tar.gz \
        "https://github.com/aovestdipaperino/tokensave/releases/download/v${TOKENSAVE_VERSION}/tokensave-v${TOKENSAVE_VERSION}-${ASSET}-linux.tar.gz"; \
    echo "${SHA}  /tmp/tokensave.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/tokensave.tar.gz -C /out; \
    install -m 0755 /out/tokensave /out/tokensave; \
    /out/tokensave --version

# Stage 2: distroless runtime. Carries only the per-arch mreview
# binary GoReleaser dropped in the build context + the verified
# tokensave binary from stage 1.
FROM gcr.io/distroless/static:nonroot AS runtime

ARG TARGETPLATFORM
COPY --chown=65532:65532 ${TARGETPLATFORM}/mreview /mreview
COPY --from=tokensave --chown=65532:65532 /out/tokensave /usr/local/bin/tokensave

# OCI labels are written by GoReleaser at build time (see
# .goreleaser.yaml dockers_v2.annotations), so this Dockerfile stays
# free of build-arg sprawl and works identically for any tag/version.

# OCI exec form so the binary gets argv[0] rather than "sh -c".
ENTRYPOINT ["/mreview"]

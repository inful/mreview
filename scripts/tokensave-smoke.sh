#!/usr/bin/env bash
#
# Tokensave MCP contract smoke test.
#
# Verifies the three things the Dockerfile's bundled-binary
# contract depends on:
#
#   1. The pinned release URL serves a binary that matches the
#      pinned SHA256. If this fails, the Dockerfile's build
#      would also fail (it uses the same checksum); failing here
#      surfaces the issue in CI before we push an image.
#
#   2. The binary runs (`--version` exits 0, prints the pinned
#      version). If the upstream tag is yanked or the release
#      asset is replaced with a stub, we catch it here.
#
#   3. The `serve` subcommand exists and accepts `--path`,
#      which is the exact shape mreview passes when it spawns
#      the subprocess (see internal/tokensave/mcp.go).
#
# This script intentionally does NOT exercise the full MCP
# handshake. The harness-side Go tests in
# internal/tokensave/mcp_test.go pin the spawn config (Command,
# Args, Name, ConnectTimeout, Optional); the runtime handshake
# itself runs every time a consumer invokes `mreview review` in
# CI, which is the integration test that matters.
#
# Usage:
#   scripts/tokensave-smoke.sh <version>
#
# Exits 0 on success, non-zero on any check failure.
set -euo pipefail

VERSION="${1:?usage: tokensave-smoke.sh <version>}"

# These checksums MUST stay in sync with the ARG block in
# Dockerfile and Dockerfile.debug. Update all four together
# whenever the tokensave version pin rolls forward; the
# upstream SHA256SUMS file at the release URL is the source of
# truth (https://github.com/aovestdipaperino/tokensave/releases/download/v<VERSION>/SHA256SUMS).
readonly SHA_LINUX_AMD64="184612db16800e384a1bcdc7fadcc53fa73bda70240f9c0416ac4b88c7e924fb"
readonly SHA_LINUX_ARM64="7de5c95b51d508f39a83d9420ad0700502de107a61dbb38dad3d008d4f6f4261"
readonly SHA_MACOS_ARM64="4b0809faccc14da7970ad6835d299cfd630fc050c2fd92f393342944c5572d0a"

case "$(uname -s)/$(uname -m)" in
    Linux/x86_64)        SHA="${SHA_LINUX_AMD64}"; ASSET="x86_64";  OS_ASSET="linux" ;;
    Linux/aarch64)       SHA="${SHA_LINUX_ARM64}"; ASSET="aarch64"; OS_ASSET="linux" ;;
    Darwin/arm64)        SHA="${SHA_MACOS_ARM64}"; ASSET="aarch64"; OS_ASSET="macos" ;;
    *)
        # Darwin/x86_64 and Windows are not supported by this
        # script — the Docker image is linux-only, so we only
        # verify the platforms that matter for the bundle.
        echo "FAIL: unsupported platform $(uname -s)/$(uname -m)" >&2
        echo "      (script supports Linux/{x86_64,aarch64} and Darwin/arm64)" >&2
        exit 1
        ;;
esac

URL="https://github.com/aovestdipaperino/tokensave/releases/download/v${VERSION}/tokensave-v${VERSION}-${ASSET}-${OS_ASSET}.tar.gz"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

echo "→ Downloading ${URL}"
curl -fsSL --retry 3 -o "${TMPDIR}/tokensave.tar.gz" "${URL}"

echo "→ Verifying SHA256"
echo "${SHA}  ${TMPDIR}/tokensave.tar.gz" | sha256sum -c -

echo "→ Extracting"
tar -xzf "${TMPDIR}/tokensave.tar.gz" -C "${TMPDIR}"
chmod +x "${TMPDIR}/tokensave"

echo "→ Smoke 1: --version"
VERSION_OUT="$("${TMPDIR}/tokensave" --version)"
echo "  ${VERSION_OUT}"
if ! grep -q "${VERSION}" <<<"${VERSION_OUT}"; then
    echo "FAIL: --version output did not contain ${VERSION}" >&2
    exit 1
fi

echo "→ Smoke 2: serve --help exposes --path"
HELP_OUT="$("${TMPDIR}/tokensave" serve --help 2>&1 || true)"
if ! grep -q -- "--path" <<<"${HELP_OUT}"; then
    echo "FAIL: 'tokensave serve --help' did not mention --path" >&2
    echo "--- captured help output ---" >&2
    echo "${HELP_OUT}" >&2
    echo "--- end ---" >&2
    exit 1
fi

echo "→ Smoke 3: binary launches and accepts --path"
# We don't drive the MCP handshake (the runtime does that, with
# proper JSON-RPC framing). We just confirm the subprocess
# starts and reaches its first stderr/log line without
# crashing on argv parsing.
LAUNCH_OUT="$(timeout 3 "${TMPDIR}/tokensave" serve --path "${TMPDIR}" 2>&1 || true)"
if grep -qi "unknown argument\|invalid flag\|unrecognized" <<<"${LAUNCH_OUT}"; then
    echo "FAIL: tokensave rejected --path:" >&2
    echo "${LAUNCH_OUT}" >&2
    exit 1
fi

echo "OK: tokensave ${VERSION} smoke test passed on $(uname -s)/$(uname -m)"

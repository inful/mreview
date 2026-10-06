#!/usr/bin/env bash
# scripts/verify-license.sh
#
# Verifies that the project's LICENSE file is byte-identical to the
# canonical AGPL-3.0 reference copy (scripts/AGPL-3.0-canonical.txt).
#
# Why this exists
# ---------------
# A repo review (token-saved on 2026-10-06) found that the LICENSE
# file had been substantively edited — word substitutions, a typo
# ("you it directly" instead of "you directly"), and an entire
# paragraph from §11 replaced with text from §7. The AGPL-3.0 text
# is the project's legal contract; "changing it is not allowed" per
# its own header. A byte-equality gate catches this class of bug
# before it ships.
#
# What it does
# ------------
# 1. Diff LICENSE against scripts/AGPL-3.0-canonical.txt.
# 2. Exit 0 if identical; exit 1 with a useful diff on divergence.
#
# The script is intentionally network-free. The canonical copy is
# checked into the repo so CI doesn't need to fetch from gnu.org.
#
# Usage
# -----
#   scripts/verify-license.sh
#   make verify-license
#
# Overriding the LICENSE path (for the shell test only):
#   LICENSE_PATH=/tmp/test-license scripts/verify-license.sh
#
# Exits 0 on match, 1 on diff, 2 on a setup error.

set -euo pipefail

# Resolve the repo root. The script lives in scripts/, so the
# canonical copy and LICENSE are one level up by default. Allow
# overrides for the shell test.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CANONICAL="${CANONICAL_PATH:-${REPO_ROOT}/scripts/AGPL-3.0-canonical.txt}"
LICENSE_FILE="${LICENSE_PATH:-${REPO_ROOT}/LICENSE}"

if [[ ! -r "${CANONICAL}" ]]; then
    echo "verify-license: canonical copy not found at ${CANONICAL}" >&2
    echo "                 (was it deleted or moved?)" >&2
    exit 2
fi

if [[ ! -r "${LICENSE_FILE}" ]]; then
    echo "verify-license: LICENSE not found at ${LICENSE_FILE}" >&2
    exit 2
fi

# diff exits 0 on identical, 1 on diff, 2 on error. We want to
# distinguish "diff" from "error" so the shell test can target
# the right exit code.
set +e
DIFF_OUTPUT="$(diff -u "${CANONICAL}" "${LICENSE_FILE}")"
DIFF_RC=$?
set -e

if [[ ${DIFF_RC} -eq 0 ]]; then
    echo "OK: LICENSE matches canonical AGPL-3.0 (${LICENSE_FILE})"
    exit 0
fi

if [[ ${DIFF_RC} -eq 1 ]]; then
    echo "FAIL: LICENSE differs from canonical AGPL-3.0" >&2
    echo "" >&2
    echo "      canonical: ${CANONICAL}" >&2
    echo "      current:   ${LICENSE_FILE}" >&2
    echo "" >&2
    echo "--- diff (first 40 lines) ---" >&2
    echo "${DIFF_OUTPUT}" | head -40 >&2
    echo "--- end diff ---" >&2
    echo "" >&2
    echo "If this is an intentional change, update both files together" >&2
    echo "and add a 'fix(license): ...' commit explaining why." >&2
    exit 1
fi

# DIFF_RC == 2 means diff itself errored (e.g. file vanished between
# the [[ -r ]] check and now). Treat as a setup error.
echo "verify-license: diff failed (rc=${DIFF_RC})" >&2
exit 2

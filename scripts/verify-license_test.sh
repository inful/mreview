#!/usr/bin/env bash
# scripts/verify-license_test.sh
#
# Shell-level test for scripts/verify-license.sh.
#
# Three cases, each as a tiny sub-script so a failure in one doesn't
# mask the others:
#
#   1. Match: a temp LICENSE that's a copy of the canonical → exit 0.
#   2. Mismatch: a temp LICENSE with one word changed → exit 1, and
#      the diff output appears on stderr.
#   3. Missing LICENSE: temp LICENSE_PATH that doesn't exist → exit 2.
#
# Run from anywhere:
#   scripts/verify-license_test.sh
#   make test-license
#
# Exits 0 if all three pass; non-zero on the first failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERIFIER="${REPO_ROOT}/scripts/verify-license.sh"
CANONICAL="${REPO_ROOT}/scripts/AGPL-3.0-canonical.txt"

if [[ ! -x "${VERIFIER}" ]]; then
    echo "FAIL: ${VERIFIER} is not executable (chmod +x)" >&2
    exit 1
fi
if [[ ! -r "${CANONICAL}" ]]; then
    echo "FAIL: canonical copy missing at ${CANONICAL}" >&2
    exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

PASS=0
FAIL=0

# --- Case 1: identical LICENSE → exit 0 ---------------------------------------
echo "→ Case 1: identical LICENSE → expect exit 0"
cp "${CANONICAL}" "${TMPDIR}/LICENSE.good"
LICENSE_PATH="${TMPDIR}/LICENSE.good" "${VERIFIER}" >/dev/null 2>&1
RC=$?
if [[ ${RC} -eq 0 ]]; then
    echo "  PASS (exit 0)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: expected exit 0, got ${RC}" >&2
    LICENSE_PATH="${TMPDIR}/LICENSE.good" "${VERIFIER}" >&2 || true
    FAIL=$((FAIL + 1))
fi

# --- Case 2: tampered LICENSE → exit 1 + diff on stderr -----------------------
echo "→ Case 2: tampered LICENSE → expect exit 1, diff on stderr"
cp "${CANONICAL}" "${TMPDIR}/LICENSE.bad"
# Make a substantive change that mirrors the kind of corruption the
# review caught: substitute a word in the §0 definition of "propagate".
sed -i.bak 's/make you directly or secondarily/make you it directly or secondarily/' \
    "${TMPDIR}/LICENSE.bad"
rm -f "${TMPDIR}/LICENSE.bad.bak"

set +e
STDERR_CAPTURE="$(
    LICENSE_PATH="${TMPDIR}/LICENSE.bad" "${VERIFIER}" 2>&1 1>/dev/null
)"
RC=$?
set -e

if [[ ${RC} -ne 1 ]]; then
    echo "  FAIL: expected exit 1, got ${RC}" >&2
    LICENSE_PATH="${TMPDIR}/LICENSE.bad" "${VERIFIER}" >&2 || true
    FAIL=$((FAIL + 1))
elif [[ "${STDERR_CAPTURE}" != *"FAIL"* ]] || [[ "${STDERR_CAPTURE}" != *"differs"* ]]; then
    echo "  FAIL: stderr did not mention FAIL/differs" >&2
    echo "  --- captured stderr ---" >&2
    echo "${STDERR_CAPTURE}" >&2
    echo "  --- end ---" >&2
    FAIL=$((FAIL + 1))
elif [[ "${STDERR_CAPTURE}" != *"you it directly"* ]]; then
    echo "  FAIL: diff did not show the tampered word" >&2
    echo "  --- captured stderr ---" >&2
    echo "${STDERR_CAPTURE}" >&2
    echo "  --- end ---" >&2
    FAIL=$((FAIL + 1))
else
    echo "  PASS (exit 1, diff visible)"
    PASS=$((PASS + 1))
fi

# --- Case 3: missing LICENSE → exit 2 ---------------------------------------
echo "→ Case 3: missing LICENSE → expect exit 2"
set +e
LICENSE_PATH="${TMPDIR}/LICENSE.missing" "${VERIFIER}" >/dev/null 2>&1
RC=$?
set -e
if [[ ${RC} -eq 2 ]]; then
    echo "  PASS (exit 2)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: expected exit 2, got ${RC}" >&2
    LICENSE_PATH="${TMPDIR}/LICENSE.missing" "${VERIFIER}" >&2 || true
    FAIL=$((FAIL + 1))
fi

# --- Summary ----------------------------------------------------------------
echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [[ ${FAIL} -ne 0 ]]; then
    exit 1
fi
exit 0

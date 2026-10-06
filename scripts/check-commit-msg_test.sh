#!/usr/bin/env bash
# scripts/check-commit-msg_test.sh
#
# Shell-level test for scripts/check-commit-msg.sh.
#
# Six cases. Each one writes a candidate commit message
# to a temp file, invokes the script with that path, and
# asserts the expected exit code. The test exits non-zero
# on the first failure.
#
# Run from anywhere:
#   scripts/check-commit-msg_test.sh
#   make test-commit-msg
#
# Exits 0 if all six pass; non-zero on the first failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT_DIR}/check-commit-msg.sh"

if [[ ! -x "${SCRIPT}" ]]; then
    echo "FAIL: ${SCRIPT} is not executable (chmod +x)" >&2
    exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

PASS=0
FAIL=0

# Helper: write $1 to a temp file, run the script, assert exit
# code equals $2, optionally assert stderr contains $3. Sets
# PASS / FAIL in the parent scope.
run_case() {
    local message="$1"
    local want_rc="$2"
    local want_stderr_contains="${3:-}"

    local msgfile="${TMPDIR}/msg.$$.$RANDOM.txt"
    printf '%s\n' "${message}" > "${msgfile}"

    set +e
    local stderr
    stderr="$("${SCRIPT}" "${msgfile}" 2>&1 1>/dev/null)"
    local got_rc=$?
    set -e

    if [[ ${got_rc} -ne ${want_rc} ]]; then
        echo "  FAIL: message=$(printf '%q' "${message}"): rc=${got_rc}, want ${want_rc}" >&2
        echo "  --- stderr ---" >&2
        echo "${stderr}" >&2
        echo "  --- end ---" >&2
        FAIL=$((FAIL + 1))
        return
    fi

    if [[ -n "${want_stderr_contains}" ]] && [[ "${stderr}" != *"${want_stderr_contains}"* ]]; then
        echo "  FAIL: message=$(printf '%q' "${message}"): stderr missing '${want_stderr_contains}'" >&2
        echo "  --- stderr ---" >&2
        echo "${stderr}" >&2
        echo "  --- end ---" >&2
        FAIL=$((FAIL + 1))
        return
    fi

    PASS=$((PASS + 1))
}

# --- Case 1: conventional commit -------------------------------------------
echo "→ Case 1: conventional commit → expect exit 0"
run_case "feat: add X" 0
if [[ ${FAIL} -eq 0 ]]; then echo "  PASS"; fi

# --- Case 2: conventional commit with scope -------------------------------
echo "→ Case 2: conventional commit with scope → expect exit 0"
run_case "feat(cli): add X" 0
if [[ ${FAIL} -eq 0 ]]; then echo "  PASS"; fi

# --- Case 3: conventional commit with breaking change --------------------
echo "→ Case 3: conventional commit with breaking change → expect exit 0"
run_case "feat!: drop X" 0
if [[ ${FAIL} -eq 0 ]]; then echo "  PASS"; fi

# --- Case 4: bad message --------------------------------------------------
echo "→ Case 4: bad message → expect exit 1 + 'Conventional Commits' in stderr"
run_case "add X" 1 "Conventional Commits"
if [[ ${FAIL} -eq 0 ]]; then echo "  PASS"; fi

# --- Case 5: merge message ------------------------------------------------
echo "→ Case 5: merge message → expect exit 0"
run_case "Merge branch 'main' into feature" 0
if [[ ${FAIL} -eq 0 ]]; then echo "  PASS"; fi

# --- Case 6: revert message -----------------------------------------------
echo "→ Case 6: revert message → expect exit 0"
run_case 'Revert "feat: drop X"' 0
if [[ ${FAIL} -eq 0 ]]; then echo "  PASS"; fi

# --- Case 7: unknown type -------------------------------------------------
echo "→ Case 7: unknown type 'bogus' → expect exit 1"
run_case "bogus: add X" 1
if [[ ${FAIL} -eq 0 ]]; then echo "  PASS"; fi

# --- Case 8: first commit (no HEAD) ---------------------------------------
# This case is special: the script reads the current git
# repo's HEAD count. A brand-new repo with no commits has
# HEAD=0, and the script should treat that as the "first
# commit on a brand-new repo" exception and exit 0.
#
# We exercise this by pointing GIT_DIR at a fresh
# `git init` repo. Setting GIT_DIR (rather than cd-ing)
# keeps the test in the same shell so PASS / FAIL
# counters propagate correctly.
echo "→ Case 8: first commit on brand-new repo → expect exit 0"
FRESH_REPO="${TMPDIR}/emptyrepo"
git init -q "${FRESH_REPO}"
GIT_DIR="${FRESH_REPO}/.git" run_case "feat: first commit" 0
if [[ ${FAIL} -eq 0 ]]; then echo "  PASS"; fi

# --- Summary ----------------------------------------------------------------
echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [[ ${FAIL} -ne 0 ]]; then
    exit 1
fi
exit 0

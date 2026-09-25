---
title: Go test patterns
description: Conventions for table-driven tests, sub-tests, and test fixtures
---

# Go test patterns

- **Table-driven tests**: use anonymous structs with `name` as the first field. Drives readability when cases diverge and produces clear `-run` filter targets.
- **Sub-tests via `t.Run`**: prefer over flat test functions. Produces better `-v` output and lets you target one case (`-run TestFoo/case_42`).
- **Test fixtures next to code**: `foo.go` → `foo_test.go`. Use `testdata/` only for large fixtures, golden files, or fixtures shared across packages.
- **Race detector in CI**: `go test -race` should be a separate CI stage that fails the build. Don't bundle it with normal `go test` — it doubles wall time and tends to be skipped on slow paths.
- **No `time.Sleep`**: sleeping in tests is fragile and slow. Use channels, `sync.WaitGroup`, or `poll` helpers (e.g. `assert.Eventually`).
- **`t.Parallel()` for slow tests**: mark tests that touch I/O as parallel so the suite finishes faster, but skip it for tests that mutate shared package-level state.
- **Helper functions**: call `t.Helper()` at the top of every test helper so failure messages report the caller's line, not the helper's.

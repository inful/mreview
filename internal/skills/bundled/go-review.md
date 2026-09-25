---
title: Go code review
description: General patterns to look for when reviewing Go code
---

# Go code review checklist

When reviewing Go code, look for:

- **Error wrapping**: errors that flow up the call stack should use `%w`, not `fmt.Errorf("...%s...", err)`. The former preserves the error chain for `errors.Is` / `errors.As`; the latter silently breaks it.
- **Goroutine leaks**: every `go func()` must have a clear exit path. Watch for goroutines blocked on channels that are never closed, or `time.Sleep` without context cancellation.
- **Context propagation**: handlers should accept `context.Context` as the first parameter and propagate it down. Look for goroutines / DB calls / HTTP requests that don't pass ctx through.
- **Resource cleanup**: `defer rows.Close()` is necessary but not sufficient. SQL row iterators return errors on `Next()` that callers often miss — pair it with `defer rows.Err()`.
- **Race conditions**: shared state mutations without mutex protection. Run `go test -race` to surface them.
- **Slice aliasing**: appending to a slice that was passed in can mutate the caller's slice. Use the full slice expression `s[low:high:cap]` when taking a sub-slice you intend to append to.

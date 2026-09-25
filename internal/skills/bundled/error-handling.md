---
title: Error handling
description: Conventions for wrapping, logging, and propagating errors
---

# Error handling conventions

- **Wrap with `%w`**: `fmt.Errorf("loading config: %w", err)`. Never `fmt.Errorf("loading config: %s", err)` — that breaks the error chain so `errors.Is(err, os.ErrNotExist)` returns false downstream.
- **Sentinel errors at the package level**: `var ErrNotFound = errors.New("not found")`. Callers branch with `errors.Is(err, ErrNotFound)`. Place at the top of the file, prefixed with `Err`.
- **Custom error types for structured info**: implement `Error()` and add fields for context (e.g. `*NotFoundError{Path: "...", UID: 42}`). Callers use `errors.As` to extract.
- **Don't log + return**: pick one. Logging at every layer floods output; returning without logging loses info. Library code returns; command code logs at the top.
- **Context for errors**: `fmt.Errorf("user %d: %w", uid, err)`. The prefix helps log searches and gives the reader a breadcrumb.
- **Don't compare error strings**: `err.Error() == "not found"` breaks the moment the message changes. Use `errors.Is` / `errors.As` exclusively.
- **Empty errors are bugs**: `if err != nil` should always be paired with handling. `//nolint:errcheck` is a smell; reach for `defer func() { _ = f.Close() }()` only when the caller has explicitly accepted the risk.

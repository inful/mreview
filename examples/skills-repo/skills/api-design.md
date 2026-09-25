---
title: API design guidelines
description: Team-specific conventions for designing HTTP and RPC APIs
---

# API design guidelines

These are our team's conventions on top of the bundled Go
review checklist. When reviewing an API change, look for:

## URL paths

- **kebab-case** for path segments: `/api/v1/user-profiles/42`, not
  `/api/v1/userProfiles/42`.
- **Plural nouns** for collections: `/api/v1/users`, not `/api/v1/user`.
- **No verbs in paths**: the HTTP method is the verb. `/api/v1/users`
  + `POST` is a create; `/api/v1/users/{id}` + `DELETE` is a delete.
- **Trailing slashes forbidden**: every route omits the trailing
  slash. Middleware redirects them away and they confuse clients.

## Status codes

- `200 OK` for successful reads / updates.
- `201 Created` with a `Location` header for successful creates.
- `204 No Content` for successful deletes.
- `400 Bad Request` for client-side validation failures.
- `401 Unauthorized` for missing / invalid auth.
- `403 Forbidden` for valid auth + insufficient permissions.
- `404 Not Found` for missing resources.
- `409 Conflict` for state-machine violations (e.g. "user already
  exists").
- `422 Unprocessable Entity` for well-formed but semantically
  invalid payloads.
- `429 Too Many Requests` for rate limiting.
- `500 Internal Server Error` only for unexpected conditions —
  never for client errors.

## JSON conventions

- **snake_case** for field names (matches our database columns).
- **`id` field** for resource identifiers, not `Id` or `ID`.
- **ISO-8601 timestamps** in UTC: `"2024-03-15T14:30:00Z"`.
- **Pagination** via `cursor` + `limit` query parameters. Cursor is
  opaque; clients should not parse it.

## Versioning

- URL prefix `/api/vN/` for breaking changes.
- New fields may be added without bumping the version. Removing or
  renaming fields requires a new version.

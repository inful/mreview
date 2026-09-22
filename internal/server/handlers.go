package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// timeNow is a package-level indirection so tests can pin the
// wall clock when asserting throttle behavior.
var timeNow = time.Now

// webhookPayload is the subset of GitLab's merge_request payload
// we care about. Other fields are ignored — the upstream schema
// is large and grows over time.
type webhookPayload struct {
	ObjectKind string `json:"object_kind"`
	EventType  string `json:"event_type"`
	Project    struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		IID    int     `json:"iid"`
		Action string  `json:"action"`
		Labels []label `json:"labels"`
	} `json:"object_attributes"`
}

// label is the subset of GitLab's label object we need. Title
// is the only field that matters — we don't care about color,
// description, or scope for label-filter purposes.
type label struct {
	Title string `json:"title"`
}

// makeWebhookHandler builds the /webhook HTTP handler.
//
//   - X-Gitlab-Token: shared secret (HMAC compare)
//   - X-Gitlab-Event: must be "Merge Request Hook"
//   - payload: GitLab merge_request JSON
//
// Responses:
//
//   - 401: missing / wrong X-Gitlab-Token
//   - 400: malformed body, unsupported event, missing fields
//   - 202: accepted (queued for review)
//   - 204: silently ignored (label filter / ignored action / non-MR event
//     / throttled duplicate delivery)
//   - 503: queue full (GitLab will retry)
func makeWebhookHandler(cfg Config, pool *Pool, logger *slog.Logger, throttle *throttle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := r.Header.Get("X-Gitlab-Token")
		if token == "" {
			logger.Debug("webhook: missing X-Gitlab-Token header")
			http.Error(w, "missing X-Gitlab-Token", http.StatusUnauthorized)
			return
		}
		if !verifyToken(token, cfg.WebhookSecret) {
			logger.Warn("webhook: token mismatch",
				"remote", r.RemoteAddr,
				"path", r.URL.Path,
			)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		event := r.Header.Get("X-Gitlab-Event")
		if event != "Merge Request Hook" {
			// Silently accept + ignore non-MR events. GitLab sends
			// many event types; we only care about merge_request.
			logger.Debug("webhook: ignoring non-MR event", "event", event)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
		if err != nil {
			logger.Warn("webhook: read body", "err", err.Error())
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		defer func() { _ = r.Body.Close() }()

		var payload webhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			logger.Warn("webhook: invalid JSON", "err", err.Error())
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if payload.ObjectKind != "merge_request" {
			http.Error(w, "unsupported object_kind", http.StatusBadRequest)
			return
		}
		if payload.Project.PathWithNamespace == "" || payload.ObjectAttributes.IID == 0 {
			http.Error(w, "missing project or iid", http.StatusBadRequest)
			return
		}
		if !shouldReview(payload.ObjectAttributes.Action) {
			// Not an action we care about (e.g. "close",
			// "approved", "merge") — accept silently.
			logger.Debug("webhook: ignoring action", "action", payload.ObjectAttributes.Action)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if cfg.RequiredLabel != "" && !hasLabel(payload.ObjectAttributes.Labels, cfg.RequiredLabel) {
			// Operator-configured gate: only review MRs that
			// carry the required label. Acknowledge delivery
			// (204) so GitLab doesn't retry.
			logger.Debug("webhook: ignoring MR without required label",
				"required", cfg.RequiredLabel,
				"project", payload.Project.PathWithNamespace,
				"iid", payload.ObjectAttributes.IID,
			)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Throttle: skip rapid-fire duplicates within the
		// configured window. The in-flight / just-finished
		// review already covers the latest commit — GitLab's
		// retry will fire again after the window expires.
		if throttle.shouldSkip(payload.Project.PathWithNamespace, payload.ObjectAttributes.IID, timeNow()) {
			logger.Debug("webhook: throttled duplicate",
				"project", payload.Project.PathWithNamespace,
				"iid", payload.ObjectAttributes.IID,
				"window", cfg.ThrottleWindow.String(),
			)
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprintln(w, "throttled")
			return
		}

		job := Job{
			Project:   payload.Project.PathWithNamespace,
			IID:       payload.ObjectAttributes.IID,
			Action:    payload.ObjectAttributes.Action,
			EventType: event,
		}

		if err := pool.Submit(job); err != nil {
			if errors.Is(err, ErrQueueFull) {
				logger.Warn("webhook: queue full; rejecting with 503",
					"project", job.Project, "iid", job.IID,
					"queue_depth", pool.Depth(),
					"queue_capacity", pool.Capacity(),
				)
				http.Error(w, "queue full", http.StatusServiceUnavailable)
				return
			}
			logger.Error("webhook: submit", "err", err.Error())
			http.Error(w, "submit failed", http.StatusInternalServerError)
			return
		}

		logger.Info("webhook: accepted",
			"project", job.Project,
			"iid", job.IID,
			"action", job.Action,
		)
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintln(w, "queued")
	}
}

// shouldReview returns true for GitLab merge_request actions we
// want to review.
//
// "open"     — new MR
// "reopen"   — previously closed MR re-opened
// "update"   — new commit pushed; the most common trigger
//
// We deliberately exclude "approved", "merge", "close" — the
// review is meaningless after the decision was made.
func shouldReview(action string) bool {
	switch action {
	case "open", "reopen", "update":
		return true
	default:
		return false
	}
}

// hasLabel reports whether any label in the slice has the given
// title. Comparison is exact and case-sensitive (GitLab preserves
// label casing; "Bug" and "bug" are different labels).
//
// An empty labels slice returns false. A nil slice returns false.
func hasLabel(labels []label, title string) bool {
	for _, l := range labels {
		if l.Title == title {
			return true
		}
	}
	return false
}

// verifyToken compares two strings in constant time. Returns
// false when either is empty.
//
// crypto/subtle.ConstantTimeCompare returns 1 when the inputs
// are identical (same length AND same content); 0 otherwise.
// The constant-time property holds as long as the secret isn't
// logged alongside the comparison (it isn't here).
func verifyToken(provided, expected string) bool {
	if provided == "" || expected == "" {
		return false
	}
	if len(provided) != len(expected) {
		// Run a same-length compare so the wall-clock cost is
		// independent of whether the lengths match.
		_ = subtle.ConstantTimeCompare([]byte(expected), []byte(expected))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

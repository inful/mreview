package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewClient_NormalizeBaseURL covers the auto-prepend
// behaviour for --gitlab-url. The operator's CI passes
// $CI_SERVER_URL (the web-UI base, e.g. "https://gitlab.com"),
// but the client-go SDK constructs requests by appending
// "projects/.../merge_requests/..." verbatim to whatever URL
// it gets. Without the prepend, the request URL becomes
// "<base>/projects/..." which GitLab returns 404 for (it
// expects "<base>/api/v4/...").
//
// The test uses httptest to spin up a fake GitLab server and
// asserts the SDK's request path is /api/v4/..., not /...
func TestNewClient_NormalizeBaseURL(t *testing.T) {
	// Minimal valid MergeRequest JSON the SDK will accept.
	// Only the fields our wrapper reads are populated.
	mrBody, _ := json.Marshal(map[string]any{
		"id":            1,
		"iid":           1,
		"project_id":    1,
		"title":         "test",
		"description":   "",
		"state":         "opened",
		"web_url":       "https://example.com/1",
		"source_branch": "s",
		"target_branch": "t",
		"author": map[string]any{
			"id": 1, "username": "u", "name": "u", "avatar_url": "",
		},
	})

	cases := []struct {
		name            string
		input           string
		wantPath        string // exact path the SDK should hit on the fake server
		wantLogContains string // "" means: no log line expected
	}{
		{
			name:            "web-UI base URL gets /api/v4 prepended",
			input:           "https://gitlab.example.com", // will be replaced with fake server URL by sub-test
			wantPath:        "/api/v4/projects/foo/bar/merge_requests/1",
			wantLogContains: "auto-prepended /api/v4",
		},
		{
			name:            "trailing slash is tolerated",
			input:           "https://gitlab.example.com/", // sub-test appends trailing / first
			wantPath:        "/api/v4/projects/foo/bar/merge_requests/1",
			wantLogContains: "auto-prepended /api/v4",
		},
		{
			name:            "self-hosted with port is preserved",
			input:           "https://gitlab.mgmlab.net:8443",
			wantPath:        "/api/v4/projects/foo/bar/merge_requests/1",
			wantLogContains: "auto-prepended /api/v4",
		},
		{
			name:            "API root URL is passed through unchanged (no log line)",
			input:           "https://gitlab.example.com/api/v4",
			wantPath:        "/api/v4/projects/foo/bar/merge_requests/1",
			wantLogContains: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Capture the slog output so we can assert the
			// "auto-prepended" debug line (or its absence).
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			// Fake GitLab server: records the request path
			// and returns a minimal valid MergeRequest.
			var hitPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hitPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.Copy(w, bytes.NewReader(mrBody))
			}))
			defer srv.Close()

			// The sub-test's "input" is a template; replace
			// the host:port with the fake server's address
			// so the SDK actually calls our server.
			// We assume the template starts with
			// "https://gitlab.example.com" or
			// "https://gitlab.mgmlab.net:8443".
			inputURL := tc.input
			switch {
			case strings.HasPrefix(inputURL, "https://gitlab.example.com"):
				inputURL = strings.Replace(inputURL, "https://gitlab.example.com", srv.URL, 1)
			case strings.HasPrefix(inputURL, "https://gitlab.mgmlab.net:8443"):
				inputURL = strings.Replace(inputURL, "https://gitlab.mgmlab.net:8443", srv.URL, 1)
			default:
				t.Fatalf("unhandled test URL prefix: %q", inputURL)
			}

			client, err := NewClient(inputURL, "test-token", RetryConfig{}, logger)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}

			// Issue a FetchMR via the wrapper. The wrapper
			// calls the SDK which builds the request URL
			// from the baseURL + the standard GitLab
			// /projects/.../merge_requests/... path.
			_, _ = client.FetchMR(context.Background(), "foo/bar", 1)

			// Assert the SDK hit the /api/v4/... path.
			if hitPath == "" {
				t.Fatalf("fake server received no request; client baseURL: %s",
					client.baseURL)
			}
			if hitPath != tc.wantPath {
				t.Errorf("SDK hit %q, want %q (client baseURL: %s)",
					hitPath, tc.wantPath, client.baseURL)
			}

			// Assert the log line (or its absence).
			if tc.wantLogContains != "" {
				if !strings.Contains(logBuf.String(), tc.wantLogContains) {
					t.Errorf("expected log to contain %q, got: %s",
						tc.wantLogContains, logBuf.String())
				}
			} else {
				if strings.Contains(logBuf.String(), "auto-prepended") {
					t.Errorf("API-root URL should NOT trigger auto-prepend; log: %s",
						logBuf.String())
				}
			}

			// Also assert the stored baseURL is the
			// normalised form, not the operator's input.
			// This catches a regression where the
			// auto-prepend fires for the SDK but isn't
			// reflected in the Client's own baseURL field.
			expectedNormalized := strings.TrimRight(srv.URL, "/") + "/api/v4"
			if tc.input == "https://gitlab.example.com/api/v4" {
				expectedNormalized = strings.TrimRight(inputURL, "/")
			}
			if client.baseURL != expectedNormalized {
				t.Errorf("client.baseURL = %q, want %q", client.baseURL, expectedNormalized)
			}
		})
	}
}

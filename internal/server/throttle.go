package server

import (
	"sync"
	"time"
)

// throttle is a per-(project, iid) cooldown used by the webhook
// handler to skip rapid-fire duplicate deliveries. When a webhook
// fires for the same MR inside the configured window, the
// handler returns 202 without enqueuing a review — the in-flight
// review (if any) covers the latest commit anyway because GitLab
// re-fires only on user-driven pushes, not on each commit.
//
// Bounded map: when the size hits maxSize, the oldest entry is
// evicted to keep memory under control on busy GitLab instances.
// A background sweeper (started in Server.Run) also drops entries
// older than the window so stale MRs don't accumulate.
type throttle struct {
	mu      sync.Mutex
	entries map[string]throttleEntry
	window  time.Duration
	maxSize int
}

type throttleEntry struct {
	at      time.Time // last review-accepted timestamp
	project string
	iid     int
}

// newThrottle constructs a throttle. window == 0 means
// "throttling disabled" — shouldSkip always returns false. The
// caller is responsible for translating that into a no-op server.
//
// maxSize caps memory; defaults are sensible for a single GitLab
// instance seeing up to ~10k MRs per day.
func newThrottle(window time.Duration, maxSize int) *throttle {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &throttle{
		entries: make(map[string]throttleEntry),
		window:  window,
		maxSize: maxSize,
	}
}

// shouldSkip returns true if (project, iid) was accepted within
// the throttle window. If not, it records the current time and
// returns false.
//
// When the map reaches maxSize, the oldest entry is evicted to
// make room (FIFO).
func (t *throttle) shouldSkip(project string, iid int, now time.Time) bool {
	if t.window <= 0 {
		return false
	}
	key := throttleKey(project, iid)

	t.mu.Lock()
	defer t.mu.Unlock()

	if prev, ok := t.entries[key]; ok {
		if now.Sub(prev.at) < t.window {
			return true
		}
	}

	// Evict the oldest entry when at capacity so memory stays
	// bounded even under sustained MR churn.
	if len(t.entries) >= t.maxSize {
		var oldestKey string
		var oldestAt time.Time
		for k, v := range t.entries {
			if oldestKey == "" || v.at.Before(oldestAt) {
				oldestKey = k
				oldestAt = v.at
			}
		}
		if oldestKey != "" {
			delete(t.entries, oldestKey)
		}
	}

	t.entries[key] = throttleEntry{
		at:      now,
		project: project,
		iid:     iid,
	}
	return false
}

// sweep removes entries older than the window. Run periodically
// from a goroutine so the map self-cleans even on idle instances.
func (t *throttle) sweep(now time.Time) int {
	if t.window <= 0 {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	removed := 0
	for k, v := range t.entries {
		if now.Sub(v.at) > t.window {
			delete(t.entries, k)
			removed++
		}
	}
	return removed
}

// Size returns the number of tracked entries. Exposed for tests
// and for the /healthz endpoint if we add it later.
func (t *throttle) Size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

func throttleKey(project string, iid int) string {
	return project + "!" + itoa(iid)
}

// itoa is a tiny non-allocating int-to-string. We avoid strconv
// here because this path is on the hot webhook receive.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

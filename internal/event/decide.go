package event

import (
	"fmt"
)

// Decision is the verdict of the per-event guard. Proceed == true
// means the review should run; Proceed == false means the caller
// should log Reason at debug level and exit 0.
//
// Reason is human-readable and log-line-shaped — it carries the
// why for operators reading logs (e.g. "draft MR (use --on-drafts=run
// to override)").
type Decision struct {
	Proceed bool
	Reason  string
}

// Prefer is the CLI flag value type for --on-drafts / --on-push.
// Empty string defaults to PreferSkip via Decide.
type Prefer string

const (
	// PreferRun forces the review even when the default is skip.
	PreferRun Prefer = "run"

	// PreferSkip forces a clean exit 0 even when the default is
	// proceed. Useful as a kill-switch during incident response.
	PreferSkip Prefer = "skip"
)

// String satisfies flag.Value / kong.TextUnmarshaler; the enum
// tag in cmd/mreview/review.go drives the public name.
func (p Prefer) String() string {
	return string(p)
}

// Decide evaluates the event against the operator's preferences
// and returns the verdict. Pure function — no logging, no I/O —
// so tests can assert every combination without stubbing loggers.
//
// The decision matrix (see #41's locked table):
//
//	Source               | default    | override
//	---------------------|------------|--------------------
//	SourceLocal          | proceed    | n/a
//	SourceMergeRequest   | proceed    | skip when MRDraft && onDrafts != run
//	SourceWeb/API/Sched. | proceed    | n/a
//	SourcePush           | skip       | proceed when onPush == run
//	everything else      | skip       | n/a
//
// Reason is non-empty when Proceed is false and explains why;
// empty when Proceed is true.
func Decide(e Event, onDrafts, onPush Prefer) Decision {
	// Local invocation always proceeds — the developer wants
	// to see what mreview thinks of their MR.
	if e.Source == SourceLocal {
		return Decision{Proceed: true}
	}

	// Supported sources short-circuit before the override check:
	// drafts need the operator's explicit opt-in; everything
	// else in this bucket proceeds.
	if e.IsSupported() {
		if e.Source == SourceMergeRequestEvent && e.MRDraft {
			if onDrafts == PreferRun {
				return Decision{
					Proceed: true,
					Reason:  "draft MR with --on-drafts=run override",
				}
			}
			return Decision{
				Proceed: false,
				Reason:  "draft MR (use --on-drafts=run to override)",
			}
		}
		return Decision{Proceed: true}
	}

	// Unsupported source. The only one with a real override
	// today is push; every other value is unconditionally skip.
	if e.Source == SourcePush {
		if onPush == PreferRun {
			return Decision{
				Proceed: true,
				Reason:  "push source with --on-push=run override",
			}
		}
		return Decision{
			Proceed: false,
			Reason:  "push source (use --on-push=run to override)",
		}
	}

	return Decision{
		Proceed: false,
		Reason:  fmt.Sprintf("unsupported CI source %q (review not warranted)", e.Source),
	}
}

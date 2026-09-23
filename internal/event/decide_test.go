package event

import (
	"strings"
	"testing"
)

// TestDecide_TableDriven walks every combination the issue
// promises. Each row pins a single (event, onDrafts, onPush)
// triple against an expected Proceed/Reason outcome. Adding a new
// source? Add a row here first; Decide's coverage is the
// contract.
func TestDecide_TableDriven(t *testing.T) {
	cases := []struct {
		name         string
		event        Event
		onDrafts     Prefer
		onPush       Prefer
		wantOK       bool   // Proceed?
		wantInReason string // substring expected in Reason; "" for proceed rows
	}{
		// ---- Local invocation ----
		{
			name:   "local proceeds regardless of flags",
			event:  Event{Source: SourceLocal},
			wantOK: true,
		},
		{
			name:     "local + skip-flag still proceeds (skip is a no-op locally)",
			event:    Event{Source: SourceLocal},
			onDrafts: PreferSkip,
			wantOK:   true,
		},

		// ---- Supported sources ----
		{
			name:   "merge_request non-draft proceeds",
			event:  Event{Source: SourceMergeRequestEvent, MRIID: 42},
			wantOK: true,
		},
		{
			name:         "merge_request draft skipped by default",
			event:        Event{Source: SourceMergeRequestEvent, MRIID: 42, MRDraft: true},
			wantOK:       false,
			wantInReason: "draft MR",
		},
		{
			name:         "merge_request draft + --on-drafts=run proceeds",
			event:        Event{Source: SourceMergeRequestEvent, MRIID: 42, MRDraft: true},
			onDrafts:     PreferRun,
			wantOK:       true,
			wantInReason: "draft MR",
		},
		{
			name:         "merge_request draft + --on-drafts=skip proceeds (no-op; skip is the default)",
			event:        Event{Source: SourceMergeRequestEvent, MRIID: 42, MRDraft: true},
			onDrafts:     PreferSkip,
			wantOK:       false,
			wantInReason: "draft MR",
		},
		{
			name:   "web proceeds",
			event:  Event{Source: SourceWeb},
			wantOK: true,
		},
		{
			name:   "api proceeds",
			event:  Event{Source: SourceAPI},
			wantOK: true,
		},
		{
			name:   "schedule proceeds",
			event:  Event{Source: SourceSchedule},
			wantOK: true,
		},

		// ---- Push (unsupported, but has an opt-in) ----
		{
			name:         "push skipped by default",
			event:        Event{Source: SourcePush},
			wantOK:       false,
			wantInReason: "push source",
		},
		{
			name:         "push + --on-push=run proceeds",
			event:        Event{Source: SourcePush},
			onPush:       PreferRun,
			wantOK:       true,
			wantInReason: "push source",
		},
		{
			name:         "push + --on-push=skip still skips (skip is the default)",
			event:        Event{Source: SourcePush},
			onPush:       PreferSkip,
			wantOK:       false,
			wantInReason: "push source",
		},

		// ---- Other unsupported sources ----
		{
			name:         "trigger skipped",
			event:        Event{Source: SourceTrigger},
			wantOK:       false,
			wantInReason: "unsupported CI source",
		},
		{
			name:         "pipeline skipped",
			event:        Event{Source: SourcePipeline},
			wantOK:       false,
			wantInReason: "unsupported CI source",
		},
		{
			name:         "parent_pipeline skipped",
			event:        Event{Source: SourceParentPipeline},
			wantOK:       false,
			wantInReason: "unsupported CI source",
		},
		{
			name:         "webide skipped",
			event:        Event{Source: SourceWebIDE},
			wantOK:       false,
			wantInReason: "unsupported CI source",
		},
		{
			name:         "chat skipped",
			event:        Event{Source: SourceChat},
			wantOK:       false,
			wantInReason: "unsupported CI source",
		},
		{
			name:         "ondemand_dast skipped",
			event:        Event{Source: SourceOnDemandDAST},
			wantOK:       false,
			wantInReason: "unsupported CI source",
		},
		{
			name:         "security_orchestration_policy skipped",
			event:        Event{Source: SourceSecurityOrchestration},
			wantOK:       false,
			wantInReason: "unsupported CI source",
		},
		{
			name:         "external_pull_request_event skipped",
			event:        Event{Source: SourceExternalPullRequestEvent},
			wantOK:       false,
			wantInReason: "unsupported CI source",
		},

		// ---- Forward compat: unknown source must default to skip ----
		{
			name:         "unknown source skipped",
			event:        Event{Source: Source("future-value-added-by-gitlab")},
			wantOK:       false,
			wantInReason: "unsupported CI source",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.event, c.onDrafts, c.onPush)
			if d.Proceed != c.wantOK {
				t.Errorf("Proceed = %v, want %v (Reason=%q)",
					d.Proceed, c.wantOK, d.Reason)
			}
			// A proceed-via-override row keeps Reason non-empty
			// (so operators see *why* the review ran on what
			// would normally be a skip event). A plain proceed
			// row has no reason at all.
			if c.wantOK && c.wantInReason == "" && d.Reason != "" {
				t.Errorf("Reason = %q on a plain-Proceed row; should be empty",
					d.Reason)
			}
			if c.wantInReason != "" {
				if !strings.Contains(d.Reason, c.wantInReason) {
					t.Errorf("Reason = %q, want substring %q",
						d.Reason, c.wantInReason)
				}
			}
		})
	}
}

// TestDecide_EmptyPreferDefaultsToSkip verifies that an empty
// Prefer string (the CLI's "flag not set" case) behaves like
// PreferSkip. Otherwise unset flags would silently default to
// "proceed" — wrong for drafts.
func TestDecide_EmptyPreferDefaultsToSkip(t *testing.T) {
	d := Decide(
		Event{Source: SourceMergeRequestEvent, MRIID: 1, MRDraft: true},
		Prefer(""),
		Prefer(""),
	)
	if d.Proceed {
		t.Errorf("empty Prefer should default to skip; got Proceed=true (Reason=%q)",
			d.Reason)
	}
}

// TestPreferString_RoundTrip pins the string form so the kong
// enum tag binding doesn't silently break.
func TestPreferString_RoundTrip(t *testing.T) {
	cases := map[Prefer]string{
		PreferRun:  "run",
		PreferSkip: "skip",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("Prefer(%q).String() = %q, want %q", string(p), got, want)
		}
	}
}

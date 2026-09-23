package event

import (
	"testing"
)

// clearCIEnv unsets every GitLab CI variable the package reads.
// Tests call this first so a CI run from a real GitLab pipeline
// (where the env is pre-populated) doesn't leak into Detect().
//
// t.Setenv("KEY", "") is documented as setting the variable to
// the empty string — semantically distinct from unset. To
// genuinely unset we delete the var via os.Unsetenv wrapped in a
// cleanup. t.Setenv("KEY", "") + the package's own check for the
// empty string works because CI_PIPELINE_SOURCE is the gate: if
// it's empty, we return SourceLocal without reading anything else.
func clearCIEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CI_PIPELINE_SOURCE", "")
	t.Setenv("CI_MERGE_REQUEST_IID", "")
	t.Setenv("CI_MERGE_REQUEST_DRAFT", "")
}

func TestDetect_NoEnv_ReturnsLocal(t *testing.T) {
	clearCIEnv(t)
	e := Detect()
	if e.Source != SourceLocal {
		t.Errorf("Source = %q, want %q", e.Source, SourceLocal)
	}
	if e.FromEnv {
		t.Errorf("FromEnv = true, want false (no env read)")
	}
	if e.MRIID != 0 || e.MRDraft {
		t.Errorf("MRIID/MRDraft should be zero when Source is Local; got MRIID=%d MRDraft=%v",
			e.MRIID, e.MRDraft)
	}
}

func TestDetect_MR_Event_NonDraft(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("CI_PIPELINE_SOURCE", string(SourceMergeRequestEvent))
	t.Setenv("CI_MERGE_REQUEST_IID", "42")
	t.Setenv("CI_MERGE_REQUEST_DRAFT", "false")

	e := Detect()
	if e.Source != SourceMergeRequestEvent {
		t.Errorf("Source = %q, want %q", e.Source, SourceMergeRequestEvent)
	}
	if e.MRIID != 42 {
		t.Errorf("MRIID = %d, want 42", e.MRIID)
	}
	if e.MRDraft {
		t.Errorf("MRDraft = true, want false")
	}
	if !e.FromEnv {
		t.Errorf("FromEnv = false, want true")
	}
}

func TestDetect_MR_Event_Draft(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("CI_PIPELINE_SOURCE", string(SourceMergeRequestEvent))
	t.Setenv("CI_MERGE_REQUEST_IID", "7")
	t.Setenv("CI_MERGE_REQUEST_DRAFT", "true")

	e := Detect()
	if e.MRDraft != true {
		t.Errorf("MRDraft = false, want true (env value is 'true')")
	}
}

func TestDetect_DraftAcceptsCapitalTrue(t *testing.T) {
	// GitLab's docs don't promise a case convention; be lenient.
	clearCIEnv(t)
	t.Setenv("CI_PIPELINE_SOURCE", string(SourceMergeRequestEvent))
	t.Setenv("CI_MERGE_REQUEST_DRAFT", "True")

	e := Detect()
	if !e.MRDraft {
		t.Errorf("MRDraft = false for env value %q, want true (case-insensitive match)",
			"True")
	}
}

func TestDetect_BadMRIID_FallsBackToZero(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("CI_PIPELINE_SOURCE", string(SourceMergeRequestEvent))
	t.Setenv("CI_MERGE_REQUEST_IID", "not-a-number")

	e := Detect()
	if e.MRIID != 0 {
		t.Errorf("MRIID = %d, want 0 for non-numeric env value", e.MRIID)
	}
}

func TestDetect_NonMR_Event_PopulatesSource(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("CI_PIPELINE_SOURCE", "schedule")

	e := Detect()
	if e.Source != SourceSchedule {
		t.Errorf("Source = %q, want %q", e.Source, SourceSchedule)
	}
	if e.MRIID != 0 {
		t.Errorf("MRIID should be 0 for non-MR sources; got %d", e.MRIID)
	}
	if !e.FromEnv {
		t.Errorf("FromEnv = false, want true")
	}
}

func TestIsSupported_TableDriven(t *testing.T) {
	cases := []struct {
		src       Source
		supported bool
	}{
		// Supported.
		{SourceLocal, true},
		{SourceMergeRequestEvent, true},
		{SourceWeb, true},
		{SourceAPI, true},
		{SourceSchedule, true},

		// Unsupported.
		{SourcePush, false},
		{SourceTrigger, false},
		{SourcePipeline, false},
		{SourceParentPipeline, false},
		{SourceWebIDE, false},
		{SourceChat, false},
		{SourceOnDemandDAST, false},
		{SourceSecurityOrchestration, false},
		{SourceExternalPullRequestEvent, false},

		// Unknown source — must not be supported (forward-compat
		// means new GitLab values should default to "skip" until
		// explicitly added to the supported list).
		{Source("future-value"), false},
	}
	for _, c := range cases {
		t.Run(string(c.src), func(t *testing.T) {
			e := Event{Source: c.src}
			if got := e.IsSupported(); got != c.supported {
				t.Errorf("IsSupported(%q) = %v, want %v", c.src, got, c.supported)
			}
		})
	}
}

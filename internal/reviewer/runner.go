package reviewer

import (
	"context"

	"github.com/sausheong/harness/runtime"
)

// harnessRunner is the production Runner — wraps
// harness's runtime.Runtime and dispatches RunSync calls to
// it. The orchestrator depends on the Runner interface
// (defined in orchestrator.go) so tests can substitute a
// fake without spinning up a real LLM.
type harnessRunner struct {
	rt *runtime.Runtime
}

// NewHarnessRunnerFromRuntime builds a Runner from a built
// runtime. Callers are responsible for building the runtime
// (typically via runtime.BuildRuntime); the orchestrator
// takes the Runner interface so the test suite can swap in
// FakeRunner without touching the harness library.
func NewHarnessRunnerFromRuntime(rt *runtime.Runtime) Runner {
	return &harnessRunner{rt: rt}
}

// RunSync sends the prompt to the harness agent. The
// orchestrator passes system + user prompts; harness's
// RunSync treats them as the agent's input.
func (h *harnessRunner) RunSync(ctx context.Context, system, user string) (string, error) {
	// Harness's RunSync takes (userMsg, images). System
	// prompt is part of AgentSpec; we set it at build time.
	// The user's message carries the diff + metadata.
	_ = system
	return h.rt.RunSync(ctx, user, nil)
}

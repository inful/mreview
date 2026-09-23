package artifact

// BuildResult is the parsed `go build ./...` log. We don't
// attempt to fully parse the build output (it's free-form
// text); we keep the last N lines and a build-error
// detection flag for the prompt renderer.
//
// LastLines is the tail of build.log (capped at LastLineCount
// in the loader). BuildError is true when any line matches
// the build-error pattern (`error:` or `undefined:`); the
// prompt surfaces this as a hint.
type BuildResult struct {
	LastLines  string
	BuildError bool
}

// LastLineCount is the number of trailing lines the loader
// keeps from build.log. Local models rarely need more than
// the last 50 to identify a build break; the rest is just
// noise. Truncation is in the prompt renderer.
const LastLineCount = 50

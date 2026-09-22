package main

import (
	"os"
	"testing"

	"github.com/inful/mreview/internal/config"
)

// osWriteFile is a package-level indirection so tests can stub
// the filesystem call. (Currently unused indirection, but kept
// for future flexibility.)
var osWriteFile = os.WriteFile

// mustLoadConfig is a tiny helper that loads a config from path
// (or returns a default-populated empty file when path is "").
// Tests use it to set up config objects without repeating the
// Load boilerplate.
func mustLoadConfig(t *testing.T, path string) *config.File {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", path, err)
	}
	return cfg
}

// configFileForTest returns a config.File with all-zero values
// (no defaults applied). Tests that want to assert "zero → not
// propagated to env" use this instead of config.Load("").
func configFileForTest() config.File {
	return config.File{}
}

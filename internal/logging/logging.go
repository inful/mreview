// Package logging builds the slog.Logger used by every subcommand.
//
// The constructor is intentionally tiny: the choice is "text" (default) or
// "json" for format, and a single boolean for verbose (Debug vs Info). Tests
// can capture output by passing a bytes.Buffer.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a slog.Logger that writes to w in the requested format.
//
//   - format == "json": structured JSON line per event.
//   - format == "text" (default): human-friendly key=value text.
//   - format == anything else: returns an error so misconfiguration is loud.
//
// verbose == true unlocks the slog debug level; verbose == false keeps the
// logger at info.
func New(w io.Writer, format string, verbose bool) (*slog.Logger, error) {
	switch strings.ToLower(format) {
	case "json", "text":
	default:
		return nil, fmt.Errorf("logging: --log-format must be text or json, got %q", format)
	}
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler), nil
}

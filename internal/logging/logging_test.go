package logging_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/logging"
)

func TestNew_TextFormat_DebugLevelOff(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New(&buf, "text", false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("hello", "k", "v")
	out := buf.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "k=v") {
		t.Errorf("text log line missing expected fields: %q", out)
	}
}

func TestNew_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New(&buf, "json", false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("hello", "k", "v")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log line is not valid JSON: %v\n%s", err, buf.String())
	}
	if m["msg"] != "hello" || m["k"] != "v" {
		t.Errorf("JSON fields wrong: %+v", m)
	}
}

func TestNew_InvalidFormat(t *testing.T) {
	_, err := logging.New(&bytes.Buffer{}, "yaml", false)
	if err == nil {
		t.Fatal("expected error for unknown log format")
	}
}

func TestNew_DebugLevelOn(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New(&buf, "json", true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Debug("dmsg")
	if !strings.Contains(buf.String(), "dmsg") {
		t.Errorf("debug message was filtered out: %q", buf.String())
	}
}

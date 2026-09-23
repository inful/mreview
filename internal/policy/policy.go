package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// Load reads path and returns the parsed + compiled Policy.
// Returns an error when:
//   - the file is unreadable
//   - the YAML doesn't parse
//   - the YAML contains unknown top-level fields
//   - a severity value isn't one of info / warning / error
//   - a glob pattern fails to compile
//   - a regex pattern fails to compile
//
// Strict validation is the contract — the policy file is the
// operator's only channel for severity overrides and synthetic
// rules, so a silently-ignored field would silently disable a
// rule the operator expects to fire.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: read %q: %w", path, err)
	}
	return Parse(data)
}

// Parse is the in-memory equivalent of Load; tests use it to
// exercise the loader without filesystem I/O.
func Parse(data []byte) (*Policy, error) {
	// Decoding into a map first lets us reject unknown fields
	// at the top level without writing a parallel struct +
	// duplicate-tag dance. The map keys become the policy's
	// top-level sections; anything else is rejected.
	var raw map[string]yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		// io.EOF means "empty document" — treat as a valid
		// empty policy. Anything else is a parse error.
		if errors.Is(err, io.EOF) {
			return &Policy{}, nil
		}
		return nil, fmt.Errorf("policy: parse YAML: %w", err)
	}

	p := &Policy{}
	for key, node := range raw {
		switch key {
		case "severity_overrides":
			if err := node.Decode(&p.SeverityOverrides); err != nil {
				return nil, fmt.Errorf("policy: severity_overrides: %w", err)
			}
		case "forbid":
			if err := node.Decode(&p.Forbid); err != nil {
				return nil, fmt.Errorf("policy: forbid: %w", err)
			}
		case "require":
			if err := node.Decode(&p.Require); err != nil {
				return nil, fmt.Errorf("policy: require: %w", err)
			}
		case "labels":
			// labels is map[string]string in YAML but
			// map[string]Severity internally; decode into
			// the raw form first so we can validate each
			// value.
			var rawLabels map[string]string
			if err := node.Decode(&rawLabels); err != nil {
				return nil, fmt.Errorf("policy: labels: %w", err)
			}
			p.Labels = make(map[string]Severity, len(rawLabels))
			for k, v := range rawLabels {
				if !ValidSeverity(v) {
					return nil, fmt.Errorf(
						"policy: labels[%q]: severity %q is not one of info|warning|error",
						k, v,
					)
				}
				p.Labels[k] = Severity(v)
			}
		default:
			return nil, fmt.Errorf(
				"policy: unknown top-level field %q (allowed: severity_overrides, forbid, require, labels)",
				key,
			)
		}
	}

	if err := p.compile(); err != nil {
		return nil, err
	}
	return p, nil
}

// compile pre-validates every glob and regex in the policy.
// Failures here catch typos like `severity: errror` or
// `pattern: "[unbalanced"` before the enforcer ever sees the
// policy. Cheap and one-shot; the compiled forms live on the
// Policy for use by Enforce.
func (p *Policy) compile() error {
	for i, ov := range p.SeverityOverrides {
		if !ValidSeverity(string(ov.Severity)) {
			return fmt.Errorf(
				"policy: severity_overrides[%d]: severity %q is not one of info|warning|error",
				i, ov.Severity,
			)
		}
		if _, err := doublestar.Match(ov.Pattern, ""); err != nil {
			// Match compiles the pattern lazily; we trigger
			// a probe to surface compile errors here.
			return fmt.Errorf(
				"policy: severity_overrides[%d]: invalid glob %q: %w",
				i, ov.Pattern, err,
			)
		}
	}
	for i, fb := range p.Forbid {
		if fb.ID == "" {
			return fmt.Errorf("policy: forbid[%d]: id is required", i)
		}
		if _, err := regexp.Compile(fb.Pattern); err != nil {
			return fmt.Errorf(
				"policy: forbid[%d] %q: invalid regex %q: %w",
				i, fb.ID, fb.Pattern, err,
			)
		}
	}
	for i, rq := range p.Require {
		if rq.ID == "" {
			return fmt.Errorf("policy: require[%d]: id is required", i)
		}
		if _, err := doublestar.Match(rq.Pattern, ""); err != nil {
			return fmt.Errorf(
				"policy: require[%d] %q: invalid glob %q: %w",
				i, rq.ID, rq.Pattern, err,
			)
		}
	}
	return nil
}

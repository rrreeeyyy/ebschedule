package main

import (
	"bytes"
	"fmt"

	"github.com/pmezard/go-difflib/difflib"
	"gopkg.in/yaml.v3"
)

// mustYAML encodes v to a 2-space-indented YAML string. Panics on error,
// since v is always an in-memory struct we control and yaml.Encoder writes
// to a bytes.Buffer that can't fail; an error here would mean a programming
// bug (e.g. an unmarshalable type) we want surfaced loudly rather than
// silently producing an empty diff.
func mustYAML(v any) string {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		panic(fmt.Errorf("mustYAML encode: %w", err))
	}
	if err := enc.Close(); err != nil {
		panic(fmt.Errorf("mustYAML close: %w", err))
	}
	return buf.String()
}

// unifiedDiff renders a git-style unified diff between current and
// desired, labeling the hunks with name. Returns the empty string when
// the inputs are equal (difflib's behavior).
func unifiedDiff(name, current, desired string) string {
	d := difflib.UnifiedDiff{
		A:        difflib.SplitLines(current),
		B:        difflib.SplitLines(desired),
		FromFile: name + " (current)",
		ToFile:   name + " (desired)",
		Context:  3,
	}
	s, _ := difflib.GetUnifiedDiffString(d)
	return s
}

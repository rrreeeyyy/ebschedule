package main

import (
	"bytes"
	"encoding/json"

	"gopkg.in/yaml.v3"
)

// jsonField holds a JSON document. The YAML representation can be either a
// scalar string (legacy / explicit JSON) or a structured mapping/sequence
// (preferred — the YAML reader auto-converts to JSON on load). Internally
// the value is always stored as canonical JSON (compact, sorted keys) so
// that diff comparison is whitespace-insensitive between user input and
// AWS-returned strings.
//
// On marshal, a stored canonical JSON string is decoded back into a Go
// value and emitted as structured YAML, so dump output and import-ecschedule
// output are readable rather than embedded JSON-in-YAML.
type jsonField string

// UnmarshalYAML accepts either a scalar JSON string or a structured
// mapping / sequence and stores the canonicalized JSON internally.
// Invalid JSON is stored verbatim so validate can surface the parse
// error rather than this layer hiding it.
func (j *jsonField) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Value == "" {
			*j = ""
			return nil
		}
		canon, err := canonicalizeJSON([]byte(node.Value))
		if err != nil {
			// Not valid JSON; keep the original so validation can flag it.
			*j = jsonField(node.Value)
			return nil
		}
		*j = jsonField(canon)
		return nil
	case yaml.MappingNode, yaml.SequenceNode:
		var v any
		if err := node.Decode(&v); err != nil {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		canon, err := canonicalizeJSON(b)
		if err != nil {
			return err
		}
		*j = jsonField(canon)
		return nil
	case yaml.AliasNode:
		return j.UnmarshalYAML(node.Alias)
	default:
		// Null / document nodes: treat as empty.
		*j = ""
		return nil
	}
}

// MarshalYAML decodes the stored JSON back to a Go value so dump output
// emits structured YAML. Falls back to the verbatim string when the
// stored value isn't valid JSON (only reachable via the
// canonicalization-failure fallback in UnmarshalYAML).
func (j jsonField) MarshalYAML() (any, error) {
	if j == "" {
		return "", nil
	}
	var v any
	if err := json.Unmarshal([]byte(j), &v); err == nil {
		return v, nil
	}
	return string(j), nil
}

// jsonFieldFromAWS wraps a JSON string returned by AWS, canonicalizing it
// so diff comparison stays whitespace-insensitive. Empty input yields the
// empty jsonField; invalid JSON is stored verbatim so validate can flag it.
func jsonFieldFromAWS(s string) jsonField {
	if s == "" {
		return ""
	}
	canon, err := canonicalizeJSON([]byte(s))
	if err != nil {
		return jsonField(s)
	}
	return jsonField(canon)
}

// canonicalizeJSON normalizes a JSON byte string to compact form with sorted
// map keys. Returns the empty string for empty input.
func canonicalizeJSON(b []byte) (string, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v) // json.Marshal sorts map keys by default
	if err != nil {
		return "", err
	}
	return string(out), nil
}

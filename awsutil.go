package main

import (
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// parseTime parses an RFC3339 string into a *time.Time, returning nil for
// the empty string. Used by the YAML -> AWS direction (Schedule's
// startDate / endDate).
func parseTime(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("invalid RFC3339 time %q: %w", s, err)
	}
	return &t, nil
}

// formatTime renders a *time.Time back to RFC3339 in UTC, returning the
// empty string for nil. The AWS -> YAML direction.
func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// nilIfEmpty returns aws.String(s) for non-empty inputs, nil for empty.
// Reaches into the AWS SDK's optional-string idiom from the YAML side.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

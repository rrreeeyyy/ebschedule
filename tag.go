package main

import (
	"maps"
	"sort"
)

// trackingTagKey marks resources managed by ebschedule. -prune only deletes
// resources carrying this tag with the trackingId from the same config.
const trackingTagKey = "ebschedule-tracking-id"

// mergeTags returns a new map containing base ∪ override; override wins on
// conflict. Returns nil when both inputs are empty so YAML round-trips
// don't show a stray empty-map key.
func mergeTags(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := map[string]string{}
	maps.Copy(out, base)
	maps.Copy(out, override)
	return out
}

// reconcileTags brings `current` -> `desired ∪ {trackingTagKey: trackingValue}`.
// If trackingValue is empty, the tracking tag is left untouched.
// Tags present remotely that aren't in the desired set are removed (except
// the tracking tag when trackingValue is empty).
func reconcileTags(
	current, desired map[string]string,
	trackingValue string,
	set func(map[string]string) error,
	unset func([]string) error,
) error {
	full := map[string]string{}
	maps.Copy(full, desired)
	if trackingValue != "" {
		full[trackingTagKey] = trackingValue
	}
	toSet := map[string]string{}
	for k, v := range full {
		if cv, ok := current[k]; !ok || cv != v {
			toSet[k] = v
		}
	}
	var toUnset []string
	for k := range current {
		if _, ok := full[k]; ok {
			continue
		}
		if trackingValue == "" && k == trackingTagKey {
			continue
		}
		toUnset = append(toUnset, k)
	}
	sort.Strings(toUnset)
	if len(toSet) > 0 {
		if err := set(toSet); err != nil {
			return err
		}
	}
	if len(toUnset) > 0 {
		if err := unset(toUnset); err != nil {
			return err
		}
	}
	return nil
}

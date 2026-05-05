package main

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestConfigBusGroup(t *testing.T) {
	c := &Config{}
	if got := c.bus(); got != "default" {
		t.Errorf("bus() default = %q, want default", got)
	}
	if got := c.group(); got != "default" {
		t.Errorf("group() default = %q, want default", got)
	}
	c.EventBusName = "custom-bus"
	c.GroupName = "custom-group"
	if got := c.bus(); got != "custom-bus" {
		t.Errorf("bus() custom = %q", got)
	}
	if got := c.group(); got != "custom-group" {
		t.Errorf("group() custom = %q", got)
	}
}

func TestMergeTags(t *testing.T) {
	cases := []struct {
		name     string
		base     map[string]string
		override map[string]string
		want     map[string]string
	}{
		{"both nil", nil, nil, nil},
		{"base only", map[string]string{"a": "1"}, nil, map[string]string{"a": "1"}},
		{"override only", nil, map[string]string{"b": "2"}, map[string]string{"b": "2"}},
		{
			"override wins",
			map[string]string{"a": "1", "b": "base"},
			map[string]string{"b": "ov", "c": "3"},
			map[string]string{"a": "1", "b": "ov", "c": "3"},
		},
		{"both empty maps -> nil", map[string]string{}, map[string]string{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeTags(tc.base, tc.override)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mergeTags() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseFormatTime(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := parseTime("")
		if err != nil || got != nil {
			t.Errorf("parseTime(\"\") = (%v, %v), want (nil, nil)", got, err)
		}
		if formatTime(nil) != "" {
			t.Errorf("formatTime(nil) != empty")
		}
	})
	t.Run("round-trip RFC3339", func(t *testing.T) {
		want := "2026-06-01T03:00:00Z"
		ts, err := parseTime(want)
		if err != nil || ts == nil {
			t.Fatalf("parseTime(%q) error: %v", want, err)
		}
		if got := formatTime(ts); got != want {
			t.Errorf("round-trip: got %q want %q", got, want)
		}
	})
	t.Run("non-UTC formatted as UTC", func(t *testing.T) {
		jst, _ := time.LoadLocation("Asia/Tokyo")
		t9 := time.Date(2026, 5, 5, 9, 0, 0, 0, jst)
		got := formatTime(&t9)
		if got != "2026-05-05T00:00:00Z" {
			t.Errorf("formatTime did not normalize to UTC: %q", got)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		_, err := parseTime("yesterday")
		if err == nil {
			t.Error("expected error on invalid time")
		}
	})
}

func TestNilIfEmpty(t *testing.T) {
	if nilIfEmpty("") != nil {
		t.Error("empty string should return nil")
	}
	if got := nilIfEmpty("x"); got == nil || *got != "x" {
		t.Errorf("nilIfEmpty(\"x\") = %v", got)
	}
}

func TestExpandTemplate(t *testing.T) {
	t.Setenv("EBS_TEST_VAR", "hello")
	t.Run("env funcs", func(t *testing.T) {
		raw := []byte(`a: {{ env "EBS_TEST_VAR" }}` + "\n" + `b: {{ env "EBS_NOPE" }}`)
		out, err := expandTemplate(raw, runtimeFuncs())
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != "a: hello\nb: " {
			t.Errorf("unexpected: %q", out)
		}
	})
	t.Run("runtime must_env errors when unset", func(t *testing.T) {
		raw := []byte(`x: {{ must_env "EBS_DEFINITELY_UNSET" }}`)
		_, err := expandTemplate(raw, runtimeFuncs())
		if err == nil {
			t.Error("expected error from must_env on unset var")
		}
	})
	t.Run("validate must_env emits placeholder", func(t *testing.T) {
		raw := []byte(`x: {{ must_env "EBS_DEFINITELY_UNSET" }}`)
		out, err := expandTemplate(raw, validateFuncs())
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != `x: <env:EBS_DEFINITELY_UNSET>` {
			t.Errorf("validate placeholder mismatch: %q", out)
		}
	})
	t.Run("validate ssm placeholder", func(t *testing.T) {
		raw := []byte(`x: {{ ssm "/p/k" }}`)
		out, err := expandTemplate(raw, validateFuncs())
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != `x: <ssm:/p/k>` {
			t.Errorf("ssm placeholder mismatch: %q", out)
		}
	})
	t.Run("syntax error surfaces", func(t *testing.T) {
		raw := []byte(`{{ env \"X\" }}`)
		if _, err := expandTemplate(raw, runtimeFuncs()); err == nil {
			t.Error("expected template parse error")
		}
	})

	for _, fn := range []string{"env", "must_env", "ssm"} {
		if _, ok := runtimeFuncs()[fn]; !ok {
			t.Errorf("runtimeFuncs missing %s", fn)
		}
		if _, ok := validateFuncs()[fn]; !ok {
			t.Errorf("validateFuncs missing %s", fn)
		}
	}
}

// --- reconcileTags ---------------------------------------------------------

type fakeTagOps struct {
	setCalls   []map[string]string
	unsetCalls [][]string
	setErr     error
	unsetErr   error
}

func (f *fakeTagOps) set(tags map[string]string) error {
	cp := map[string]string{}
	for k, v := range tags {
		cp[k] = v
	}
	f.setCalls = append(f.setCalls, cp)
	return f.setErr
}
func (f *fakeTagOps) unset(keys []string) error {
	cp := append([]string(nil), keys...)
	f.unsetCalls = append(f.unsetCalls, cp)
	return f.unsetErr
}

// flatten the (single) set call into k=v sorted slice for stable assertions.
func sortedSet(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func TestReconcileTags(t *testing.T) {
	t.Run("create from scratch with tracking", func(t *testing.T) {
		f := &fakeTagOps{}
		err := reconcileTags(
			nil,
			map[string]string{"Service": "app", "Env": "prod"},
			"my-app", f.set, f.unset,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.setCalls) != 1 {
			t.Fatalf("expected 1 set call, got %d", len(f.setCalls))
		}
		got := sortedSet(f.setCalls[0])
		want := []string{"Env=prod", "Service=app", trackingTagKey + "=my-app"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("set tags = %v, want %v", got, want)
		}
		if len(f.unsetCalls) != 0 {
			t.Errorf("unexpected unset calls: %v", f.unsetCalls)
		}
	})

	t.Run("no-op when current already matches", func(t *testing.T) {
		f := &fakeTagOps{}
		current := map[string]string{
			"Service":      "app",
			trackingTagKey: "my-app",
		}
		desired := map[string]string{"Service": "app"}
		err := reconcileTags(current, desired, "my-app", f.set, f.unset)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.setCalls) != 0 || len(f.unsetCalls) != 0 {
			t.Errorf("expected no-op, set=%v unset=%v", f.setCalls, f.unsetCalls)
		}
	})

	t.Run("unset extras, keep tracking when trackingValue passed", func(t *testing.T) {
		f := &fakeTagOps{}
		current := map[string]string{
			"Service":      "app",
			"Old":          "drop-me",
			trackingTagKey: "my-app",
		}
		desired := map[string]string{"Service": "app"}
		err := reconcileTags(current, desired, "my-app", f.set, f.unset)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.setCalls) != 0 {
			t.Errorf("did not expect set, got %v", f.setCalls)
		}
		if len(f.unsetCalls) != 1 || !reflect.DeepEqual(f.unsetCalls[0], []string{"Old"}) {
			t.Errorf("unset = %v, want [[Old]]", f.unsetCalls)
		}
	})

	t.Run("update value of existing key", func(t *testing.T) {
		f := &fakeTagOps{}
		current := map[string]string{"Env": "stg"}
		desired := map[string]string{"Env": "prod"}
		err := reconcileTags(current, desired, "", f.set, f.unset)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.setCalls) != 1 || f.setCalls[0]["Env"] != "prod" {
			t.Errorf("set = %v, want Env=prod", f.setCalls)
		}
	})

	t.Run("trackingValue empty preserves existing tracking tag", func(t *testing.T) {
		// Caller didn't pass a trackingValue (e.g. dump/diff path). Even if the
		// remote already carries an ebschedule-tracking-id, we must NOT remove
		// it -- this is the "never delete other people's tracking" guard.
		f := &fakeTagOps{}
		current := map[string]string{trackingTagKey: "someone-else"}
		desired := map[string]string{}
		err := reconcileTags(current, desired, "", f.set, f.unset)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.setCalls) != 0 {
			t.Errorf("did not expect set, got %v", f.setCalls)
		}
		if len(f.unsetCalls) != 0 {
			t.Errorf("must NOT unset tracking tag when trackingValue is empty, got %v", f.unsetCalls)
		}
	})

	t.Run("trackingValue overrides stale tracking tag value", func(t *testing.T) {
		// If someone manually changed the tracking tag, the next apply should
		// reset it to our trackingID, not leave the old value alone.
		f := &fakeTagOps{}
		current := map[string]string{trackingTagKey: "stale"}
		desired := map[string]string{}
		err := reconcileTags(current, desired, "fresh", f.set, f.unset)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.setCalls) != 1 || f.setCalls[0][trackingTagKey] != "fresh" {
			t.Errorf("expected tracking tag reset to fresh, got %v", f.setCalls)
		}
	})

	t.Run("propagates set error", func(t *testing.T) {
		myErr := errors.New("boom")
		f := &fakeTagOps{setErr: myErr}
		err := reconcileTags(nil, map[string]string{"a": "1"}, "", f.set, f.unset)
		if !errors.Is(err, myErr) {
			t.Errorf("err = %v, want boom", err)
		}
	})

	t.Run("propagates unset error", func(t *testing.T) {
		myErr := errors.New("boom")
		f := &fakeTagOps{unsetErr: myErr}
		err := reconcileTags(map[string]string{"old": "1"}, nil, "", f.set, f.unset)
		if !errors.Is(err, myErr) {
			t.Errorf("err = %v, want boom", err)
		}
	})

	t.Run("unset call is sorted for determinism", func(t *testing.T) {
		f := &fakeTagOps{}
		current := map[string]string{"z": "1", "a": "1", "m": "1"}
		desired := map[string]string{}
		_ = reconcileTags(current, desired, "", f.set, f.unset)
		if len(f.unsetCalls) != 1 {
			t.Fatalf("want 1 unset call, got %d", len(f.unsetCalls))
		}
		want := []string{"a", "m", "z"}
		if !reflect.DeepEqual(f.unsetCalls[0], want) {
			t.Errorf("unset keys = %v, want %v", f.unsetCalls[0], want)
		}
	})
}

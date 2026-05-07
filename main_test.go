package main

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func writeTempYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "conf.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExpandFiles_MissingFileSentinelError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	_, err := expandFiles(missing, runtimeFuncs(), false)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, errNoConfigFiles) {
		t.Errorf("expected errNoConfigFiles sentinel, got %v", err)
	}
}

func TestExpandFiles_TemplateErrorIsNotMissingSentinel(t *testing.T) {
	// A parse error must NOT match errNoConfigFiles, so runDump won't swallow it.
	p := writeTempYAML(t, `x: {{ env \"X\" }}`)
	_, err := expandFiles(p, runtimeFuncs(), false)
	if err == nil {
		t.Fatal("expected template parse error")
	}
	if errors.Is(err, errNoConfigFiles) {
		t.Errorf("template error must not be classified as missing-file: %v", err)
	}
}

func TestLoadConfigs_StrictUnknownField(t *testing.T) {
	t.Run("unknown top-level field rejected", func(t *testing.T) {
		p := writeTempYAML(t, `
region: ap-northeast-1
tag:
  Service: app
rules:
  - name: x
    scheduleExpression: rate(1 hour)
    targets:
      - id: t
        arn: arn:aws:lambda:us-east-1:1:function:f
`)
		_, err := loadConfigs(p)
		if err == nil {
			t.Fatal("expected error for unknown field, got nil")
		}
		if !strings.Contains(err.Error(), "field tag not found") {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("unknown nested field rejected", func(t *testing.T) {
		p := writeTempYAML(t, `
region: ap-northeast-1
rules:
  - name: x
    scheduleExpresion: rate(1 hour)  # typo: missing 's'
    targets:
      - id: t
        arn: arn:aws:lambda:us-east-1:1:function:f
`)
		_, err := loadConfigs(p)
		if err == nil {
			t.Fatal("expected error for typo'd field, got nil")
		}
	})
	t.Run("happy path still loads", func(t *testing.T) {
		p := writeTempYAML(t, `
region: ap-northeast-1
rules:
  - name: x
    scheduleExpression: rate(1 hour)
    targets:
      - id: t
        arn: arn:aws:lambda:us-east-1:1:function:f
`)
		cfgs, err := loadConfigs(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfgs) != 1 || len(cfgs[0].Rules) != 1 {
			t.Errorf("unexpected cfgs: %+v", cfgs)
		}
	})
}

func TestJSONField_RoundTripScalar(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // canonical form expected
	}{
		{"compact JSON", `{"a":1,"b":2}`, `{"a":1,"b":2}`},
		{"unsorted keys get sorted", `{"b":2,"a":1}`, `{"a":1,"b":2}`},
		{"whitespace stripped", `{ "a" : 1 ,  "b": 2 }`, `{"a":1,"b":2}`},
		{"nested + array", `{"src":["x","y"],"d":{"k":"v"}}`, `{"d":{"k":"v"},"src":["x","y"]}`},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var holder struct {
				F jsonField `yaml:"f"`
			}
			yamlIn := "f: '" + strings.ReplaceAll(tc.in, "'", "''") + "'"
			if tc.in == "" {
				yamlIn = "f: ''"
			}
			if err := yaml.Unmarshal([]byte(yamlIn), &holder); err != nil {
				t.Fatal(err)
			}
			if string(holder.F) != tc.want {
				t.Errorf("got %q, want %q", string(holder.F), tc.want)
			}
		})
	}
}

func TestJSONField_StructuredYAMLToCanonicalJSON(t *testing.T) {
	yamlIn := `f:
  source: [aws.s3]
  detail-type: [Object Created]
  detail:
    bucket:
      name: [my-bucket]
`
	var holder struct {
		F jsonField `yaml:"f"`
	}
	if err := yaml.Unmarshal([]byte(yamlIn), &holder); err != nil {
		t.Fatal(err)
	}
	want := `{"detail":{"bucket":{"name":["my-bucket"]}},"detail-type":["Object Created"],"source":["aws.s3"]}`
	if string(holder.F) != want {
		t.Errorf("got %q\nwant %q", string(holder.F), want)
	}
}

func TestJSONField_MarshalEmitsStructured(t *testing.T) {
	holder := struct {
		F jsonField `yaml:"f"`
	}{F: `{"k":"v","arr":[1,2]}`}
	out, err := yaml.Marshal(holder)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	// Should be structured (multi-line), not a quoted JSON string.
	if !strings.Contains(got, "k: v") || !strings.Contains(got, "- 1") {
		t.Errorf("marshal didn't produce structured YAML:\n%s", got)
	}
}

func TestJSONField_InvalidJSONStoredVerbatim(t *testing.T) {
	// validate.go relies on this so it can flag bad JSON to the user.
	yamlIn := `f: "{not valid"`
	var holder struct {
		F jsonField `yaml:"f"`
	}
	if err := yaml.Unmarshal([]byte(yamlIn), &holder); err != nil {
		t.Fatal(err)
	}
	if string(holder.F) != "{not valid" {
		t.Errorf("invalid JSON should be stored verbatim, got %q", string(holder.F))
	}
}

func TestJSONField_RoundTripParseRender(t *testing.T) {
	// Parse structured YAML, marshal back; the result should also parse back
	// to the same canonical string.
	in := `f:
  source: [aws.s3]
  detail:
    state: [running]
`
	var holder1 struct {
		F jsonField `yaml:"f"`
	}
	if err := yaml.Unmarshal([]byte(in), &holder1); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(holder1)
	if err != nil {
		t.Fatal(err)
	}
	var holder2 struct {
		F jsonField `yaml:"f"`
	}
	if err := yaml.Unmarshal(out, &holder2); err != nil {
		t.Fatal(err)
	}
	if string(holder1.F) != string(holder2.F) {
		t.Errorf("round-trip changed canonical form:\n  before: %q\n  after:  %q", holder1.F, holder2.F)
	}
}

func TestJSONFieldFromAWS_Canonicalizes(t *testing.T) {
	got := jsonFieldFromAWS(`{"b":2, "a":1}`)
	if string(got) != `{"a":1,"b":2}` {
		t.Errorf("got %q, want canonical sorted", string(got))
	}
}

func TestTargetFlag(t *testing.T) {
	t.Run("Set parses kind:name", func(t *testing.T) {
		var f targetFlag
		if err := f.Set("rule:my-rule"); err != nil {
			t.Fatal(err)
		}
		if err := f.Set("schedule:my-sched"); err != nil {
			t.Fatal(err)
		}
		if err := f.Set("rule:other"); err != nil {
			t.Fatal(err)
		}
		if !f.rules["my-rule"] || !f.rules["other"] || !f.schedules["my-sched"] {
			t.Errorf("targetFlag = %+v", f)
		}
	})
	t.Run("rejects missing colon", func(t *testing.T) {
		var f targetFlag
		if err := f.Set("plain-name"); err == nil {
			t.Error("expected error for missing kind prefix")
		}
	})
	t.Run("rejects unknown kind", func(t *testing.T) {
		var f targetFlag
		if err := f.Set("eventbus:default"); err == nil {
			t.Error("expected error for unknown kind")
		}
	})
	t.Run("rejects empty name", func(t *testing.T) {
		var f targetFlag
		if err := f.Set("rule:"); err == nil {
			t.Error("expected error for empty name")
		}
	})
}

func TestTargetFlag_Filter(t *testing.T) {
	cfg := &Config{
		Rules: []*Rule{
			{Name: "r1"}, {Name: "r2"}, {Name: "r3"},
		},
		Schedules: []*Schedule{
			{Name: "s1"}, {Name: "s2"},
		},
	}
	t.Run("inactive returns config unchanged", func(t *testing.T) {
		var f targetFlag
		got, err := f.filter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got != cfg {
			t.Errorf("inactive filter must return same pointer")
		}
	})
	t.Run("rule-only target nils Schedules", func(t *testing.T) {
		var f targetFlag
		_ = f.Set("rule:r2")
		got, err := f.filter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got.Schedules != nil {
			t.Errorf("Schedules should be nil, got %v", got.Schedules)
		}
		if len(got.Rules) != 1 || got.Rules[0].Name != "r2" {
			t.Errorf("Rules = %v, want [r2]", got.Rules)
		}
	})
	t.Run("schedule-only target nils Rules", func(t *testing.T) {
		var f targetFlag
		_ = f.Set("schedule:s1")
		got, err := f.filter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got.Rules != nil {
			t.Errorf("Rules should be nil, got %v", got.Rules)
		}
		if len(got.Schedules) != 1 || got.Schedules[0].Name != "s1" {
			t.Errorf("Schedules = %v, want [s1]", got.Schedules)
		}
	})
	t.Run("multiple kinds keeps both", func(t *testing.T) {
		var f targetFlag
		_ = f.Set("rule:r1")
		_ = f.Set("rule:r3")
		_ = f.Set("schedule:s2")
		got, err := f.filter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Rules) != 2 || len(got.Schedules) != 1 {
			t.Errorf("got Rules=%v Schedules=%v", got.Rules, got.Schedules)
		}
	})
	t.Run("unknown rule errors", func(t *testing.T) {
		var f targetFlag
		_ = f.Set("rule:nonexistent")
		_, err := f.filter(cfg)
		if err == nil || !strings.Contains(err.Error(), "rule:nonexistent not found") {
			t.Errorf("expected not-found error, got %v", err)
		}
	})
	t.Run("unknown schedule errors", func(t *testing.T) {
		var f targetFlag
		_ = f.Set("schedule:nope")
		_, err := f.filter(cfg)
		if err == nil || !strings.Contains(err.Error(), "schedule:nope not found") {
			t.Errorf("expected not-found error, got %v", err)
		}
	})
}

func TestResolveBaseFile(t *testing.T) {
	t.Run("inherits scalars and merges tags", func(t *testing.T) {
		dir := t.TempDir()
		base := filepath.Join(dir, "base.yaml")
		if err := os.WriteFile(base, []byte(`
region: ap-northeast-1
trackingId: shared-id
tags:
  Owner: platform
  Env: prod
`), 0o600); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(dir, "child.yaml")
		if err := os.WriteFile(child, []byte(`
baseFile: base.yaml
tags:
  Env: stg               # child overrides
  Service: my-app        # child adds
rules:
  - name: x
    scheduleExpression: rate(1 hour)
    targets:
      - id: t
        arn: arn:aws:lambda:us-east-1:1:function:f
`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfgs, err := loadConfigs(child)
		if err != nil {
			t.Fatal(err)
		}
		c := cfgs[0]
		if c.Region != "ap-northeast-1" || c.TrackingID != "shared-id" {
			t.Errorf("scalar inherit failed: %+v", c)
		}
		// Tags: Env overridden by child, Owner inherited, Service added.
		if c.Tags["Env"] != "stg" || c.Tags["Owner"] != "platform" || c.Tags["Service"] != "my-app" {
			t.Errorf("tag merge failed: %+v", c.Tags)
		}
	})

	t.Run("child wins on scalar conflict", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "base.yaml"), `
region: us-east-1
trackingId: parent
`)
		writeFile(t, filepath.Join(dir, "child.yaml"), `
baseFile: base.yaml
region: ap-northeast-1
trackingId: child
rules:
  - name: x
    scheduleExpression: rate(1 hour)
    targets:
      - id: t
        arn: arn:aws:lambda:us-east-1:1:function:f
`)
		cfgs, err := loadConfigs(filepath.Join(dir, "child.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfgs[0].Region != "ap-northeast-1" || cfgs[0].TrackingID != "child" {
			t.Errorf("child should win on scalar conflict: %+v", cfgs[0])
		}
	})

	t.Run("base file with rules is rejected", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "base.yaml"), `
region: us-east-1
rules:
  - name: ghost
    scheduleExpression: rate(1 hour)
    targets:
      - id: t
        arn: arn:aws:lambda:us-east-1:1:function:f
`)
		writeFile(t, filepath.Join(dir, "child.yaml"), `
baseFile: base.yaml
schedules: []
`)
		_, err := loadConfigs(filepath.Join(dir, "child.yaml"))
		if err == nil || !strings.Contains(err.Error(), "must not declare rules") {
			t.Errorf("expected base-with-rules rejection, got %v", err)
		}
	})

	t.Run("cycle detection", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "a.yaml"), `
baseFile: b.yaml
schedules: []
`)
		writeFile(t, filepath.Join(dir, "b.yaml"), `
baseFile: a.yaml
`)
		_, err := loadConfigs(filepath.Join(dir, "a.yaml"))
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Errorf("expected cycle error, got %v", err)
		}
	})

	t.Run("recursive inherit (a -> b -> c)", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "c.yaml"), `
region: ap-northeast-1
trackingId: shared-id
`)
		writeFile(t, filepath.Join(dir, "b.yaml"), `
baseFile: c.yaml
groupName: shared
`)
		writeFile(t, filepath.Join(dir, "a.yaml"), `
baseFile: b.yaml
schedules: []
`)
		cfgs, err := loadConfigs(filepath.Join(dir, "a.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		c := cfgs[0]
		if c.Region != "ap-northeast-1" || c.TrackingID != "shared-id" || c.GroupName != "shared" {
			t.Errorf("recursive inherit failed: %+v", c)
		}
	})
}

// writeFile is a small test helper that fails the test on any
// os.WriteFile error, so the tests above can stay flat.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

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

func TestTfstateFuncs(t *testing.T) {
	t.Run("validateFuncs emits placeholder", func(t *testing.T) {
		raw := []byte(`x: {{ tfstate "aws_iam_role.eb.arn" }}`)
		out, err := expandTemplate(raw, validateFuncs())
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != `x: <tfstate:aws_iam_role.eb.arn>` {
			t.Errorf("got %q", out)
		}
	})

	t.Run("runtimeFuncs without env errors with helpful message", func(t *testing.T) {
		t.Setenv(envTfstateURL, "")
		raw := []byte(`x: {{ tfstate "anything" }}`)
		_, err := expandTemplate(raw, runtimeFuncs())
		if err == nil || !strings.Contains(err.Error(), envTfstateURL) {
			t.Errorf("expected env-not-set error, got %v", err)
		}
	})

	t.Run("runtimeFuncs with bogus URL surfaces error on use", func(t *testing.T) {
		t.Setenv(envTfstateURL, "/nonexistent/terraform.tfstate")
		raw := []byte(`x: {{ tfstate "anything" }}`)
		_, err := expandTemplate(raw, runtimeFuncs())
		if err == nil {
			t.Error("expected error from bogus tfstate URL")
		}
	})

	t.Run("runtimeFuncs with valid local tfstate resolves", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "terraform.tfstate")
		body := `{
  "version": 4,
  "terraform_version": "1.5.0",
  "resources": [
    {
      "mode": "managed",
      "type": "aws_iam_role",
      "name": "eventbridge",
      "instances": [
        {"attributes": {"arn": "arn:aws:iam::1:role/test-role"}}
      ]
    }
  ]
}`
		if err := os.WriteFile(state, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envTfstateURL, state)
		raw := []byte(`role: {{ tfstate "aws_iam_role.eventbridge.arn" }}`)
		out, err := expandTemplate(raw, runtimeFuncs())
		if err != nil {
			t.Fatal(err)
		}
		want := "role: arn:aws:iam::1:role/test-role"
		if string(out) != want {
			t.Errorf("got %q, want %q", out, want)
		}
	})
}

func TestApplySummary(t *testing.T) {
	t.Run("single config: silent", func(t *testing.T) {
		cfgs := []*Config{{sourcePath: "only.yaml"}}
		out := captureStderrFn(t, func() {
			applySummary(nil, cfgs, 0, errors.New("boom"))
		})
		if out != "" {
			t.Errorf("single-config summary should be empty, got %q", out)
		}
	})
	t.Run("multi config with no prior success: silent", func(t *testing.T) {
		cfgs := []*Config{{sourcePath: "a.yaml"}, {sourcePath: "b.yaml"}}
		out := captureStderrFn(t, func() {
			applySummary(nil, cfgs, 0, errors.New("boom"))
		})
		if out != "" {
			t.Errorf("first-file failure summary should be empty, got %q", out)
		}
	})
	t.Run("multi config with partial success: prints summary", func(t *testing.T) {
		cfgs := []*Config{{sourcePath: "a.yaml"}, {sourcePath: "b.yaml"}, {sourcePath: "c.yaml"}}
		out := captureStderrFn(t, func() {
			applySummary([]string{"a.yaml"}, cfgs, 1, errors.New("boom"))
		})
		if !strings.Contains(out, "1 of 3") || !strings.Contains(out, "b.yaml") {
			t.Errorf("expected '1 of 3 ... b.yaml' summary, got %q", out)
		}
	})
}

func TestAutoResolveAccountEnv(t *testing.T) {
	t.Run("env already set: STS not consulted", func(t *testing.T) {
		t.Setenv("AWS_ACCOUNT_ID", "111111111111")
		called := false
		swapSTSResolver(t, func(_ context.Context) (string, error) {
			called = true
			return "999999999999", nil
		})
		autoResolveAccountEnv(context.Background())
		if called {
			t.Error("STS resolver should not run when env is already set")
		}
		if got := os.Getenv("AWS_ACCOUNT_ID"); got != "111111111111" {
			t.Errorf("env should be unchanged, got %s", got)
		}
	})
	t.Run("env empty: STS fills it in", func(t *testing.T) {
		t.Setenv("AWS_ACCOUNT_ID", "")
		swapSTSResolver(t, func(_ context.Context) (string, error) {
			return "888888888888", nil
		})
		autoResolveAccountEnv(context.Background())
		if got := os.Getenv("AWS_ACCOUNT_ID"); got != "888888888888" {
			t.Errorf("env should be set from STS, got %s", got)
		}
	})
	t.Run("STS error: env stays empty", func(t *testing.T) {
		t.Setenv("AWS_ACCOUNT_ID", "")
		swapSTSResolver(t, func(_ context.Context) (string, error) {
			return "", errors.New("expired credentials")
		})
		autoResolveAccountEnv(context.Background())
		if got := os.Getenv("AWS_ACCOUNT_ID"); got != "" {
			t.Errorf("env should remain empty on STS error, got %s", got)
		}
	})
	t.Run("STS returns empty: env stays empty", func(t *testing.T) {
		t.Setenv("AWS_ACCOUNT_ID", "")
		swapSTSResolver(t, func(_ context.Context) (string, error) {
			return "", nil
		})
		autoResolveAccountEnv(context.Background())
		if got := os.Getenv("AWS_ACCOUNT_ID"); got != "" {
			t.Errorf("env should remain empty on empty STS result, got %s", got)
		}
	})
}

// swapSTSResolver replaces the package-level stsAccountResolver for the
// duration of one test, restoring the original on cleanup. Lets tests cover
// the env-var-mutating path without standing up a real AWS client.
func swapSTSResolver(t *testing.T, fn func(context.Context) (string, error)) {
	t.Helper()
	orig := stsAccountResolver
	stsAccountResolver = fn
	t.Cleanup(func() { stsAccountResolver = orig })
}

// captureStderrFn synchronously captures stderr for tests that don't
// need the goroutine pipe (the apply_test.go captureStderr is fine for
// flows; this is for one-shot summary writes).
func captureStderrFn(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = orig
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	return string(buf[:n])
}

func TestJsonnetConfigLoad(t *testing.T) {
	t.Run("evaluates jsonnet and parses as Config (must_env native)", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "conf.jsonnet")
		body := `local stage = std.native("must_env")("EBS_TEST_STAGE");
{
  region: "ap-northeast-1",
  trackingId: "examples-jsonnet-" + stage,
  rules: [
    {
      name: "example-jsonnet-" + stage,
      scheduleExpression: "rate(1 hour)",
      state: if stage == "prod" then "ENABLED" else "DISABLED",
      targets: [
        {
          id: "lambda",
          arn: "arn:aws:lambda:ap-northeast-1:1:function:f-" + stage,
        },
      ],
    },
  ],
}
`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("EBS_TEST_STAGE", "prod")
		cfgs, err := loadConfigs(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfgs) != 1 {
			t.Fatalf("got %d configs, want 1", len(cfgs))
		}
		c := cfgs[0]
		if c.TrackingID != "examples-jsonnet-prod" {
			t.Errorf("trackingId = %q", c.TrackingID)
		}
		if len(c.Rules) != 1 || c.Rules[0].Name != "example-jsonnet-prod" || c.Rules[0].State != "ENABLED" {
			t.Errorf("rule = %+v", c.Rules[0])
		}
	})

	t.Run("env native returns default when unset", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "conf.jsonnet")
		body := `{
  region: std.native("env")("EBS_TEST_REGION", "us-east-1"),
  rules: [
    {
      name: "x",
      scheduleExpression: "rate(1 hour)",
      targets: [{ id: "t", arn: "arn:aws:lambda:us-east-1:1:function:f" }],
    },
  ],
}
`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		// EBS_TEST_REGION explicitly unset; env() should return the default.
		t.Setenv("EBS_TEST_REGION", "")
		_ = os.Unsetenv("EBS_TEST_REGION")
		cfgs, err := loadConfigs(p)
		if err != nil {
			t.Fatal(err)
		}
		if cfgs[0].Region != "us-east-1" {
			t.Errorf("env default not applied: region=%q", cfgs[0].Region)
		}
	})

	t.Run("must_env errors when unset", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "conf.jsonnet")
		if err := os.WriteFile(p, []byte(
			`{ region: std.native("must_env")("EBS_DEFINITELY_UNSET"), rules: [] }`,
		), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Unsetenv("EBS_DEFINITELY_UNSET")
		_, err := loadConfigs(p)
		if err == nil || !strings.Contains(err.Error(), "EBS_DEFINITELY_UNSET") {
			t.Errorf("expected must_env error, got %v", err)
		}
	})

	t.Run("must_env returns <env:NAME> placeholder under validate (offline)", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "conf.jsonnet")
		// Reference an env var that's intentionally unset. The runtime path
		// would error here; the offline path should swap in a placeholder so
		// `ebschedule validate` works without exporting AWS_ACCOUNT_ID etc.
		body := `local acct = std.native("must_env")("EBS_VALIDATE_OFFLINE_UNSET");
{
  region: "ap-northeast-1",
  trackingId: "ofl",
  rules: [{
    name: "x",
    scheduleExpression: "rate(1 hour)",
    targets: [{
      id: "lambda",
      arn: "arn:aws:lambda:ap-northeast-1:" + acct + ":function:f",
    }],
  }],
}
`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Unsetenv("EBS_VALIDATE_OFFLINE_UNSET")
		cfgs, err := loadConfigsWithFuncs(p, validateFuncs(), true)
		if err != nil {
			t.Fatalf("offline jsonnet load should not fail: %v", err)
		}
		// Placeholder must end up embedded in the synthesized ARN.
		if got := cfgs[0].Rules[0].Targets[0].Arn; !strings.Contains(got, "<env:EBS_VALIDATE_OFFLINE_UNSET>") {
			t.Errorf("expected placeholder in arn, got %q", got)
		}
	})

	t.Run("ssm returns <ssm:name> placeholder under validate (offline)", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "conf.jsonnet")
		body := `{
  region: std.native("ssm")("/ebs/test/region"),
  rules: [],
}
`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfgs, err := loadConfigsWithFuncs(p, validateFuncs(), true)
		if err != nil {
			t.Fatalf("offline ssm jsonnet load should not fail: %v", err)
		}
		if cfgs[0].Region != "<ssm:/ebs/test/region>" {
			t.Errorf("expected ssm placeholder, got %q", cfgs[0].Region)
		}
	})

	t.Run("tfstate returns <tfstate:resource> placeholder under validate (offline)", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "conf.jsonnet")
		body := `{
  region: std.native("tfstate")("module.x.aws_region"),
  rules: [],
}
`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		// EBSCHEDULE_TFSTATE_URL intentionally unset; offline path should NOT
		// emit the "set EBSCHEDULE_TFSTATE_URL" stub, just a placeholder.
		_ = os.Unsetenv(envTfstateURL)
		cfgs, err := loadConfigsWithFuncs(p, validateFuncs(), true)
		if err != nil {
			t.Fatalf("offline tfstate jsonnet load should not fail: %v", err)
		}
		if cfgs[0].Region != "<tfstate:module.x.aws_region>" {
			t.Errorf("expected tfstate placeholder, got %q", cfgs[0].Region)
		}
	})

	t.Run("syntax error surfaces with file path", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "broken.jsonnet")
		if err := os.WriteFile(p, []byte(`{ bad: [`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfigs(p)
		if err == nil || !strings.Contains(err.Error(), "jsonnet") {
			t.Errorf("expected jsonnet error, got %v", err)
		}
	})

	t.Run("tfstate native errors with helpful message when env unset", func(t *testing.T) {
		_ = os.Unsetenv(envTfstateURL)
		dir := t.TempDir()
		p := filepath.Join(dir, "conf.jsonnet")
		writeFile(t, p, `{ region: std.native("tfstate")("aws_iam_role.x.arn"), rules: [] }`)
		_, err := loadConfigs(p)
		if err == nil || !strings.Contains(err.Error(), envTfstateURL) {
			t.Errorf("expected tfstate-not-set error, got %v", err)
		}
	})

	t.Run("tfstate native resolves against a real state file", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "terraform.tfstate")
		writeFile(t, state, `{
  "version": 4,
  "terraform_version": "1.5.0",
  "resources": [
    {
      "mode": "managed",
      "type": "aws_iam_role",
      "name": "eb",
      "instances": [{"attributes": {"arn": "arn:aws:iam::1:role/eb"}}]
    }
  ]
}`)
		t.Setenv(envTfstateURL, state)
		p := filepath.Join(dir, "conf.jsonnet")
		writeFile(t, p, `{
  region: "us-east-1",
  rules: [
    {
      name: "x",
      scheduleExpression: "rate(1 hour)",
      targets: [
        {
          id: "lambda",
          arn: "arn:aws:lambda:us-east-1:1:function:f",
          roleArn: std.native("tfstate")("aws_iam_role.eb.arn"),
        },
      ],
    },
  ],
}`)
		cfgs, err := loadConfigs(p)
		if err != nil {
			t.Fatal(err)
		}
		if cfgs[0].Rules[0].Targets[0].RoleArn != "arn:aws:iam::1:role/eb" {
			t.Errorf("tfstate didn't resolve: roleArn=%q", cfgs[0].Rules[0].Targets[0].RoleArn)
		}
	})

	t.Run("ssm native error surfaces when AWS unreachable", func(t *testing.T) {
		// We can't reasonably exercise the success path without AWS creds /
		// a fake. Instead: force LoadDefaultConfig to fail by pointing the
		// SDK at an unreadable config dir, then expect a wrapped error.
		t.Setenv("AWS_CONFIG_FILE", "/dev/null/never")
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null/never")
		t.Setenv("AWS_REGION", "")
		_ = os.Unsetenv("AWS_DEFAULT_REGION")
		dir := t.TempDir()
		p := filepath.Join(dir, "conf.jsonnet")
		writeFile(t, p, `{ region: std.native("ssm")("/nonexistent"), rules: [] }`)
		_, err := loadConfigs(p)
		// AWS may or may not return an error before reaching our code,
		// depending on env. We just want to confirm the call path doesn't
		// silently succeed.
		if err == nil {
			t.Skip("AWS reachable in this environment; ssm path took the success branch")
		}
		if !strings.Contains(err.Error(), "ssm") && !strings.Contains(err.Error(), "AWS") {
			t.Errorf("expected ssm-related error, got %v", err)
		}
	})

	t.Run("local imports resolve relative to file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "_lib.libsonnet"),
			[]byte(`{ targets: [{ id: "lambda", arn: "arn:aws:lambda:us-east-1:1:function:f" }] }`),
			0o600); err != nil {
			t.Fatal(err)
		}
		main := filepath.Join(dir, "conf.jsonnet")
		body := `local lib = import "_lib.libsonnet";
{
  region: "us-east-1",
  rules: [
    {
      name: "via-import",
      scheduleExpression: "rate(1 hour)",
      targets: lib.targets,
    },
  ],
}
`
		if err := os.WriteFile(main, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfgs, err := loadConfigs(main)
		if err != nil {
			t.Fatal(err)
		}
		if cfgs[0].Rules[0].Targets[0].ID != "lambda" {
			t.Errorf("import not resolved: %+v", cfgs[0])
		}
	})
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
	maps.Copy(cp, tags)
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

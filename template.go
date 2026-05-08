package main

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"strings"
	"text/template"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/fujiwara/tfstate-lookup/tfstate"
)

// ssmHelper bundles a per-load SSM client + value cache so a single
// loadConfigs / evalJsonnet pass calls GetParameter at most once per
// distinct parameter name, even when the same key is referenced from
// multiple template / jsonnet sites. Mirrors the cache fujiwara/ssm-lookup
// uses for ecschedule's plugin path.
type ssmHelper struct {
	ctx    context.Context
	client *ssm.Client
	cache  map[string]string
}

func newSSMHelper(ctx context.Context) *ssmHelper {
	return &ssmHelper{ctx: ctx, cache: map[string]string{}}
}

// get returns the raw Parameter.Value for name (decrypted), caching by
// name. The Type is not inspected — both String and StringList come back
// as the SDK's stringly value, with comma separators preserved for the
// caller to split.
func (h *ssmHelper) get(name string) (string, error) {
	if v, ok := h.cache[name]; ok {
		return v, nil
	}
	if h.client == nil {
		cfg, err := awsconfig.LoadDefaultConfig(h.ctx)
		if err != nil {
			return "", err
		}
		h.client = ssm.NewFromConfig(cfg)
	}
	out, err := h.client.GetParameter(h.ctx, &ssm.GetParameterInput{
		Name: aws.String(name), WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("ssm:%s: %w", name, err)
	}
	v := aws.ToString(out.Parameter.Value)
	h.cache[name] = v
	return v, nil
}

// list returns the parameter value as a slice of strings, splitting on
// comma — the SSM StringList separator. A non-StringList value comes
// back as a single-element slice, which keeps the caller's iteration /
// indexing code uniform.
func (h *ssmHelper) list(name string) ([]string, error) {
	v, err := h.get(name)
	if err != nil {
		return nil, err
	}
	return strings.Split(v, ","), nil
}

// runtimeFuncs returns the FuncMap used by dump/diff/apply. Hits AWS for SSM
// and errors loudly if must_env is unset. Each call returns a fresh closure
// that owns its own lazily-constructed *ssm.Client, so test runs don't share
// state and there's no package-level singleton.
func runtimeFuncs() template.FuncMap {
	helper := newSSMHelper(context.Background())
	// ssm matches ecschedule's signature: `{{ ssm "key" }}` returns the raw
	// value (CSV for StringList), `{{ ssm "key" idx }}` returns the idx-th
	// element of the StringList. Out-of-range indices error loudly with
	// the parameter name + observed length.
	ssmFn := func(name string, index ...int) (string, error) {
		v, err := helper.get(name)
		if err != nil {
			return "", err
		}
		if len(index) == 0 {
			return v, nil
		}
		parts := strings.Split(v, ",")
		i := index[0]
		if i < 0 || i >= len(parts) {
			return "", fmt.Errorf("ssm:%s: index %d out of range (len=%d)", name, i, len(parts))
		}
		return parts[i], nil
	}
	funcs := template.FuncMap{
		"env": os.Getenv,
		"must_env": func(name string) (string, error) {
			v := os.Getenv(name)
			if v == "" {
				return "", fmt.Errorf("env var %s is not set", name)
			}
			return v, nil
		},
		"ssm": ssmFn,
	}
	addTfstateFuncs(funcs, os.Getenv(envTfstateURL))
	return funcs
}

// addTfstateFuncs registers `tfstate` (and its companions provided by
// fujiwara/tfstate-lookup) on funcs. If loc is empty, registers a stub
// that errors on use so the user gets a clear "set EBSCHEDULE_TFSTATE_URL"
// message instead of a "function not defined" template error.
func addTfstateFuncs(funcs template.FuncMap, loc string) {
	if loc == "" {
		funcs["tfstate"] = func(any) (string, error) {
			return "", fmt.Errorf("tfstate template func used but %s is not set", envTfstateURL)
		}
		return
	}
	tfFuncs, err := tfstate.FuncMap(context.Background(), loc)
	if err != nil {
		funcs["tfstate"] = func(any) (string, error) {
			return "", fmt.Errorf("tfstate (%s): %w", loc, err)
		}
		return
	}
	maps.Copy(funcs, tfFuncs)
}

// validateFuncs returns a FuncMap that never hits AWS and never errors out
// on missing values, so `validate` can run fully offline.
func validateFuncs() template.FuncMap {
	return template.FuncMap{
		"env": os.Getenv,
		"must_env": func(name string) string {
			if v := os.Getenv(name); v != "" {
				return v
			}
			return "<env:" + name + ">"
		},
		"ssm": func(name string, index ...int) string {
			if len(index) > 0 {
				return fmt.Sprintf("<ssm:%s[%d]>", name, index[0])
			}
			return "<ssm:" + name + ">"
		},
		"tfstate":  func(name string) string { return "<tfstate:" + name + ">" },
		"tfstatef": func(name string, args ...any) string { return "<tfstate:" + name + ">" },
	}
}

func expandTemplate(raw []byte, funcs template.FuncMap) ([]byte, error) {
	tmpl, err := template.New("ebschedule").Funcs(funcs).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("template parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, nil); err != nil {
		return nil, fmt.Errorf("template execute: %w", err)
	}
	return buf.Bytes(), nil
}

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fujiwara/tfstate-lookup/tfstate"
	jsonnet "github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
)

func isJsonnet(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".jsonnet" || ext == ".libsonnet"
}

// evalJsonnet runs go-jsonnet against raw source and returns the produced
// JSON. Env access is exposed via the kayac/ecspresso convention - native
// functions called from jsonnet as std.native("name")(args...). The full
// set parallels the YAML+template pipeline so users can write the same
// kind of config in either format:
//
//	std.native("env")(name, default)    // env or default
//	std.native("must_env")(name)         // env or error (offline: <env:NAME> placeholder under validate)
//	std.native("ssm")(name)              // SSM Parameter Store, decrypted (offline: <ssm:name> placeholder)
//	std.native("ssmList")(name)          // SSM StringList split on commas, returned as array
//	std.native("tfstate")(resource)      // tfstate lookup (offline: <tfstate:resource> placeholder)
//	std.native("tfstatef")(fmt, args...) // tfstate sprintf-style helper (offline: <tfstate:fmt> placeholder)
//
// ExtVar is intentionally left empty so it stays available for explicit
// user-supplied --ext-str values (matching ecspresso semantics).
//
// `offline` controls which native set is registered: validate uses the
// offline set so a config that references must_env / ssm / tfstate can
// still be checked structurally without exporting env vars or having
// AWS / state-file access. Online subcommands use the live set so
// must_env errors loudly on missing values, ssm hits AWS, etc.
func evalJsonnet(path string, raw []byte, offline bool) ([]byte, error) {
	ctx := context.Background()
	vm := jsonnet.MakeVM()
	vm.NativeFunction(jsonnetEnvFunc())
	if offline {
		vm.NativeFunction(jsonnetMustEnvFuncOffline())
		vm.NativeFunction(jsonnetSsmFuncOffline())
		vm.NativeFunction(jsonnetSsmListFuncOffline())
		for _, f := range jsonnetTfstateFuncsOffline() {
			vm.NativeFunction(f)
		}
	} else {
		vm.NativeFunction(jsonnetMustEnvFunc())
		helper := newSSMHelper()
		vm.NativeFunction(jsonnetSsmFunc(ctx, helper))
		vm.NativeFunction(jsonnetSsmListFunc(ctx, helper))
		for _, f := range jsonnetTfstateFuncs(ctx) {
			vm.NativeFunction(f)
		}
	}
	importer := &jsonnet.FileImporter{JPaths: []string{filepath.Dir(path)}}
	vm.Importer(importer)
	json, err := vm.EvaluateAnonymousSnippet(path, string(raw))
	if err != nil {
		return nil, fmt.Errorf("jsonnet: %w", err)
	}
	return []byte(json), nil
}

// --- online natives -------------------------------------------------------

// jsonnetEnvFunc registers `env(name, default)`: returns the value of the
// named env var, or `default` when unset.
func jsonnetEnvFunc() *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   "env",
		Params: []ast.Identifier{"name", "default"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("env: name must be a string")
			}
			if v, ok := os.LookupEnv(name); ok {
				return v, nil
			}
			return args[1], nil
		},
	}
}

// jsonnetMustEnvFunc registers `must_env(name)`: returns the named env var
// or errors at evaluation time when unset.
func jsonnetMustEnvFunc() *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   "must_env",
		Params: []ast.Identifier{"name"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("must_env: name must be a string")
			}
			v, ok := os.LookupEnv(name)
			if !ok {
				return nil, fmt.Errorf("env var %s is not set", name)
			}
			return v, nil
		},
	}
}

// jsonnetSsmFunc registers `ssm(name)`: SSM Parameter Store value,
// decrypted; mirrors the {{ ssm "/path" }} template func. Shares the
// per-VM cache with `ssmList` so multiple references to the same key
// only hit AWS once.
func jsonnetSsmFunc(ctx context.Context, h *ssmHelper) *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   "ssm",
		Params: []ast.Identifier{"name"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("ssm: name must be a non-empty string")
			}
			return h.get(ctx, name)
		},
	}
}

// jsonnetSsmListFunc registers `ssmList(name)`: SSM Parameter Store value
// split on commas, returned as a jsonnet array. Cleaner than indexing
// against a CSV string; for one-off element access write
// `std.native('ssmList')(name)[idx]`. A non-StringList parameter comes
// back as a one-element array, which keeps caller iteration uniform.
func jsonnetSsmListFunc(ctx context.Context, h *ssmHelper) *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   "ssmList",
		Params: []ast.Identifier{"name"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("ssmList: name must be a non-empty string")
			}
			parts, err := h.list(ctx, name)
			if err != nil {
				return nil, err
			}
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = p
			}
			return out, nil
		},
	}
}

// jsonnetTfstateFuncs registers `tfstate` and `tfstatef` (matching the
// names that fujiwara/tfstate-lookup exposes for jsonnet). When
// EBSCHEDULE_TFSTATE_URL is unset, a stub registers the same names but
// errors loudly on use - same behavior as the runtimeFuncs path.
func jsonnetTfstateFuncs(ctx context.Context) []*jsonnet.NativeFunction {
	loc := os.Getenv(envTfstateURL)
	if loc == "" {
		return []*jsonnet.NativeFunction{
			tfstateStub("tfstate", []ast.Identifier{"name"}),
			tfstateStub("tfstatef", []ast.Identifier{"format"}),
		}
	}
	funcs, err := tfstate.JsonnetNativeFuncs(ctx, "", loc)
	if err != nil {
		// State load failed; replace with stubs that surface the underlying
		// error at use time rather than at VM construction.
		msg := fmt.Sprintf("tfstate (%s): %v", loc, err)
		return []*jsonnet.NativeFunction{
			{Name: "tfstate", Params: []ast.Identifier{"name"}, Func: func([]any) (any, error) { return nil, errors.New(msg) }},
			{Name: "tfstatef", Params: []ast.Identifier{"format"}, Func: func([]any) (any, error) { return nil, errors.New(msg) }},
		}
	}
	return funcs
}

// tfstateStub backs jsonnetTfstateFuncs when EBSCHEDULE_TFSTATE_URL is
// unset: registers the function name so jsonnet's compile-time native
// resolution succeeds, but errors at call time with a message naming
// the missing env var.
func tfstateStub(name string, params []ast.Identifier) *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   name,
		Params: params,
		Func: func([]any) (any, error) {
			return nil, fmt.Errorf("%s template func used but %s is not set", name, envTfstateURL)
		},
	}
}

// --- offline natives ------------------------------------------------------
//
// Same shape as the online set above, but each returns a placeholder
// instead of hitting AWS / state files. validateFuncs() / loadConfigs
// pass `offline=true` to evalJsonnet, which switches to this set so a
// jsonnet config that references must_env / ssm / tfstate validates
// without exporting env vars or having credentials.

// jsonnetMustEnvFuncOffline mirrors the validateFuncs() behavior on the
// jsonnet side: instead of erroring when an env var is missing, return a
// `<env:NAME>` placeholder that downstream validation accepts. Lets
// `ebschedule validate` work on jsonnet configs that reference
// AWS_ACCOUNT_ID etc. without the user having to export them, the same
// way the YAML/template path already does.
func jsonnetMustEnvFuncOffline() *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   "must_env",
		Params: []ast.Identifier{"name"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("must_env: name must be a string")
			}
			if v, ok := os.LookupEnv(name); ok {
				return v, nil
			}
			return "<env:" + name + ">", nil
		},
	}
}

// jsonnetSsmFuncOffline returns `<ssm:/path>` instead of hitting AWS, so
// validate can sanity-check structure without credentials.
func jsonnetSsmFuncOffline() *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   "ssm",
		Params: []ast.Identifier{"name"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("ssm: name must be a non-empty string")
			}
			return "<ssm:" + name + ">", nil
		},
	}
}

// jsonnetSsmListFuncOffline returns a one-element array containing the
// `<ssm:/path>` placeholder, so configs that drive subnet / security
// group lists from StringList parameters still validate without hitting
// AWS. Validate downstream is permissive enough that one placeholder
// satisfies the per-element check.
func jsonnetSsmListFuncOffline() *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   "ssmList",
		Params: []ast.Identifier{"name"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("ssmList: name must be a non-empty string")
			}
			return []any{"<ssm:" + name + ">"}, nil
		},
	}
}

// jsonnetTfstateFuncsOffline returns `<tfstate:resource>` placeholders
// for both tfstate and tfstatef, matching the template path's offline
// behavior. No EBSCHEDULE_TFSTATE_URL needed under validate.
func jsonnetTfstateFuncsOffline() []*jsonnet.NativeFunction {
	return []*jsonnet.NativeFunction{
		{
			Name:   "tfstate",
			Params: []ast.Identifier{"name"},
			Func: func(args []any) (any, error) {
				name, _ := args[0].(string)
				return "<tfstate:" + name + ">", nil
			},
		},
		{
			Name:   "tfstatef",
			Params: []ast.Identifier{"format"},
			Func: func(args []any) (any, error) {
				name, _ := args[0].(string)
				return "<tfstate:" + name + ">", nil
			},
		},
	}
}

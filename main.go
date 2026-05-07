// ebschedule: a small declarative CLI for managing Amazon EventBridge Rules and
// EventBridge Scheduler Schedules in a single config file.
//
//	ebschedule [-conf FILE_OR_GLOB] [-dry-run] [-prune] <dump|diff|apply> [name-prefix]
//
// Config files are run through text/template before YAML parsing. Funcs:
//
//	{{ env "X" }}         empty if X is unset
//	{{ must_env "X" }}    errors if X is unset
//	{{ ssm "/p/k" }}      SSM Parameter Store value (decrypted)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/fujiwara/tfstate-lookup/tfstate"
	jsonnet "github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
	"github.com/pmezard/go-difflib/difflib"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// trackingTagKey marks resources managed by ebschedule. -prune only deletes
// resources carrying this tag with the trackingId from the same config.
const trackingTagKey = "ebschedule-tracking-id"

// envTfstateURL is the env var that points runtimeFuncs at a Terraform
// state file (local path, file://, s3://, http://, etc.). When set, the
// `tfstate` template func becomes available. When unset, calling
// {{ tfstate ... }} errors loudly so missing-URL isn't confused with a
// missing tfstate value.
const envTfstateURL = "EBSCHEDULE_TFSTATE_URL"

// version is set via ldflags at release time (see .goreleaser.yaml).
// "dev" is the default for local builds.
var version = "dev"

// errNoConfigFiles is returned by expandFiles when nothing matched. runDump
// checks for it to softly fall through when the user just runs `dump` without
// a pre-existing config (the bootstrap case).
var errNoConfigFiles = errors.New("no config files match")

// Exit codes:
//
//	0  success / clean
//	1  error
//	2  diff found (terraform-plan style; only `diff` uses this)
const (
	exitOK    = 0
	exitErr   = 1
	exitDrift = 2
)

// Config is the single config file format covering both Rules and Schedules.
//
// Semantics around nil vs empty:
//   - rules:    omitted -> ebschedule does not manage Rules in this config
//   - rules: [] -> ebschedule manages Rules; with -prune, deletes all tagged ones
//   - same for schedules.
type Config struct {
	Region       string            `yaml:"region,omitempty"`
	TrackingID   string            `yaml:"trackingId,omitempty"`
	Tags         map[string]string `yaml:"tags,omitempty"` // applied to every Rule and to the schedule-group (Scheduler tags only at group level)
	EventBusName string            `yaml:"eventBusName,omitempty"`
	GroupName    string            `yaml:"groupName,omitempty"`
	// BaseFile pulls inherited config (region / groupName / eventBusName /
	// trackingId / tags) from a sibling YAML so per-team / per-service
	// files can share common scaffolding. Path is resolved relative to
	// this file.
	BaseFile  string      `yaml:"baseFile,omitempty"`
	Rules     []*Rule     `yaml:"rules,omitempty"`
	Schedules []*Schedule `yaml:"schedules,omitempty"`

	sourcePath string `yaml:"-"`
}

func (c *Config) bus() string {
	if c.EventBusName != "" {
		return c.EventBusName
	}
	return "default"
}

func (c *Config) group() string {
	if c.GroupName != "" {
		return c.GroupName
	}
	return "default"
}

// --- shared "small" types --------------------------------------------------

// RetryPolicy is identical between EventBridge Rules and Scheduler Schedules.
type RetryPolicy struct {
	MaximumRetryAttempts     int32 `yaml:"maximumRetryAttempts"`
	MaximumEventAgeInSeconds int32 `yaml:"maximumEventAgeInSeconds"`
}

// DeadLetterConfig is identical between Rules and Schedules.
type DeadLetterConfig struct {
	Arn string `yaml:"arn"`
}

// SqsParameters carries the FIFO MessageGroupId for an SQS target. Used by
// both EventBridge Rules and Scheduler Schedules.
type SqsParameters struct {
	MessageGroupId string `yaml:"messageGroupId,omitempty"`
}

// CapacityProviderStrategyItem maps to ebtypes / schtypes
// CapacityProviderStrategyItem. Mutually exclusive with launchType in the
// AWS API; that constraint is enforced in validate.go.
type CapacityProviderStrategyItem struct {
	CapacityProvider string `yaml:"capacityProvider"`
	Base             int32  `yaml:"base,omitempty"`
	Weight           int32  `yaml:"weight,omitempty"`
}

// PlacementConstraint matches the AWS ECS placement-constraint shape
// (e.g. distinctInstance / memberOf with a Cluster Query Language
// expression). Same shape on Rule and Schedule SDKs.
type PlacementConstraint struct {
	Type       string `yaml:"type"` // distinctInstance | memberOf
	Expression string `yaml:"expression,omitempty"`
}

// PlacementStrategy matches the AWS ECS placement-strategy shape
// (random / spread / binpack with an optional field like
// "attribute:ecs.availability-zone").
type PlacementStrategy struct {
	Type  string `yaml:"type"` // random | spread | binpack
	Field string `yaml:"field,omitempty"`
}

// KeyValuePair holds a Name / Value pair, used for ECS RunTask Tags
// passed through to the launched task.
type KeyValuePair struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value,omitempty"`
}

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

func (j jsonField) MarshalYAML() (any, error) {
	if j == "" {
		return "", nil
	}
	var v any
	if err := json.Unmarshal([]byte(j), &v); err == nil {
		return v, nil
	}
	// Stored value isn't valid JSON (only happens via the
	// canonicalization-failure fallback in UnmarshalYAML); emit verbatim
	// so validate can still surface the parse error to the user.
	return string(j), nil
}

// SageMakerPipelineParameters supplies pipeline parameters when invoking a
// SageMaker pipeline as a target. Same shape on Rules and Schedules.
type SageMakerPipelineParameters struct {
	PipelineParameterList []SageMakerPipelineParameter `yaml:"pipelineParameterList,omitempty"`
}

// SageMakerPipelineParameter is one (Name, Value) pair in
// SageMakerPipelineParameters.PipelineParameterList.
type SageMakerPipelineParameter struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// --- main ------------------------------------------------------------------

// targetFlag implements flag.Value for repeatable `-target KIND:NAME`.
// KIND must be "rule" or "schedule"; the explicit prefix lets a Rule and
// a Schedule with the same name coexist unambiguously.
type targetFlag struct {
	rules     map[string]bool
	schedules map[string]bool
}

func (t *targetFlag) String() string {
	if t == nil {
		return ""
	}
	parts := make([]string, 0, len(t.rules)+len(t.schedules))
	for n := range t.rules {
		parts = append(parts, "rule:"+n)
	}
	for n := range t.schedules {
		parts = append(parts, "schedule:"+n)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (t *targetFlag) Set(v string) error {
	kind, name, ok := strings.Cut(v, ":")
	if !ok || name == "" {
		return fmt.Errorf("-target must be KIND:NAME (e.g. rule:my-rule), got %q", v)
	}
	switch kind {
	case "rule":
		if t.rules == nil {
			t.rules = map[string]bool{}
		}
		t.rules[name] = true
	case "schedule":
		if t.schedules == nil {
			t.schedules = map[string]bool{}
		}
		t.schedules[name] = true
	default:
		return fmt.Errorf("-target kind must be 'rule' or 'schedule', got %q", kind)
	}
	return nil
}

// active reports whether at least one -target was set.
func (t *targetFlag) active() bool {
	return t != nil && (len(t.rules) > 0 || len(t.schedules) > 0)
}

// filter returns a copy of cfg restricted to only the targeted resources.
// Sections not referenced by any target are nil'd so callers don't touch
// them at all (no apply, no prune-eligible scan).
//
// Returns an error if a -target names a resource the config doesn't define;
// silent skip would mask typos.
func (t *targetFlag) filter(cfg *Config) (*Config, error) {
	if !t.active() {
		return cfg, nil
	}
	out := *cfg

	if len(t.rules) > 0 {
		seen := map[string]bool{}
		var rules []*Rule
		for _, r := range cfg.Rules {
			if t.rules[r.Name] {
				rules = append(rules, r)
				seen[r.Name] = true
			}
		}
		for n := range t.rules {
			if !seen[n] {
				return nil, fmt.Errorf("-target rule:%s not found in config", n)
			}
		}
		out.Rules = rules
	} else {
		out.Rules = nil // no rule targets => skip Rules entirely
	}

	if len(t.schedules) > 0 {
		seen := map[string]bool{}
		var scheds []*Schedule
		for _, s := range cfg.Schedules {
			if t.schedules[s.Name] {
				scheds = append(scheds, s)
				seen[s.Name] = true
			}
		}
		for n := range t.schedules {
			if !seen[n] {
				return nil, fmt.Errorf("-target schedule:%s not found in config", n)
			}
		}
		out.Schedules = scheds
	} else {
		out.Schedules = nil
	}
	return &out, nil
}

// tagFilterFlag implements flag.Value for repeatable `-tag KEY=VALUE`. The
// resulting map is AND-matched against each remote resource's tag set.
type tagFilterFlag map[string]string

func (t *tagFilterFlag) String() string {
	if t == nil || *t == nil {
		return ""
	}
	parts := make([]string, 0, len(*t))
	for k, v := range *t {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (t *tagFilterFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("-tag must be KEY=VALUE, got %q", v)
	}
	if *t == nil {
		*t = tagFilterFlag{}
	}
	(*t)[k] = val
	return nil
}

func main() {
	var (
		confPath    string
		dryRun      bool
		prune       bool
		showVersion bool
		dumpTags    tagFilterFlag
		autoApprove bool
		targets     targetFlag
	)
	flag.StringVar(&confPath, "conf", "ebschedule.yaml", "config file or glob (e.g. 'config/*.yaml')")
	flag.BoolVar(&dryRun, "dry-run", false, "do not actually apply")
	flag.BoolVar(&prune, "prune", false, "delete tracked resources absent from config")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Var(&dumpTags, "tag", "(dump) only emit Rules with this tag; repeatable; AND across all (KEY=VALUE)")
	flag.BoolVar(&autoApprove, "auto-approve", false, "(apply) skip the interactive confirmation prompt")
	flag.Var(&targets, "target", "(diff/apply) restrict to KIND:NAME (rule:foo or schedule:bar); repeatable")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage:
  ebschedule [-conf FILE_OR_GLOB] [-dry-run] [-prune] [-auto-approve] [-target KIND:NAME]... <dump|diff|apply|validate> [name-prefix]
  ebschedule [-conf FILE_OR_GLOB] [-tag KEY=VALUE]... dump [name-prefix]
  ebschedule [-conf FILE_OR_GLOB] [-dry-run] run -rule NAME
  ebschedule import-ecschedule [-in FILE] [-account NUM] [-region REGION] [-tracking-id ID]
  ebschedule -version

Config files run through text/template before YAML parsing. Funcs:
  {{ env "X" }}         empty if X is unset
  {{ must_env "X" }}    errors if X is unset
  {{ ssm "/p/k" }}      SSM Parameter Store value (decrypted)

Exit codes:
  0  success / no drift
  1  error
  2  diff found (only emitted by 'diff')`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if showVersion {
		fmt.Println("ebschedule", version)
		os.Exit(exitOK)
	}
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(exitErr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := os.Stdout
	switch args[0] {
	case "dump":
		prefix := ""
		if len(args) > 1 {
			prefix = args[1]
		}
		autoResolveAccountEnv(ctx)
		check(runDump(ctx, out, confPath, prefix, dumpTags))
	case "diff":
		autoResolveAccountEnv(ctx)
		cfgs, err := loadConfigs(confPath)
		check(err)
		drift := false
		for _, cfg := range cfgs {
			scoped, err := targets.filter(cfg)
			check(err)
			if len(cfgs) > 1 {
				fmt.Fprintf(out, "# %s\n", scoped.sourcePath)
			}
			if scoped.Rules != nil {
				d, err := diffRules(ctx, out, scoped)
				check(err)
				drift = drift || d
			}
			if scoped.Schedules != nil {
				d, err := diffSchedules(ctx, out, scoped)
				check(err)
				drift = drift || d
			}
		}
		if drift {
			os.Exit(exitDrift)
		}
	case "apply":
		if targets.active() && prune {
			check(fmt.Errorf("-target and -prune are mutually exclusive"))
		}
		autoResolveAccountEnv(ctx)
		cfgs, err := loadConfigs(confPath)
		check(err)
		// Pre-flight runs even in -dry-run because dry-run still issues AWS
		// reads (Describe / List for the no-op detection); failing fast on
		// missing / expired creds beats letting an obscure mid-stream error
		// surface several calls in.
		if err := preflightCheck(ctx, cfgs); err != nil {
			check(fmt.Errorf("pre-flight: %w", err))
		}
		// Verify every referenced ECS task definition actually exists
		// before any EventBridge mutation, so a typo / deleted revision
		// surfaces with a clean error rather than after a partial apply.
		// Mirrors ecschedule's validateTaskDefinition behavior.
		if err := verifyTaskDefinitionsForCfgs(ctx, cfgs); err != nil {
			check(fmt.Errorf("pre-flight: %w", err))
		}
		if !dryRun && !autoApprove && stdinIsTTY() {
			if !confirmApply(os.Stderr, os.Stdin) {
				fmt.Fprintln(os.Stderr, "aborted")
				os.Exit(exitErr)
			}
		}
		applied := []string{}
		for i, cfg := range cfgs {
			scoped, err := targets.filter(cfg)
			if err != nil {
				applySummary(applied, cfgs, i, err)
				check(err)
			}
			if len(cfgs) > 1 {
				fmt.Fprintf(out, "# %s\n", scoped.sourcePath)
			}
			if scoped.Rules != nil {
				if err := applyRules(ctx, out, scoped, dryRun, prune); err != nil {
					applySummary(applied, cfgs, i, err)
					check(err)
				}
			}
			if scoped.Schedules != nil {
				if err := applySchedules(ctx, out, scoped, dryRun, prune); err != nil {
					applySummary(applied, cfgs, i, err)
					check(err)
				}
			}
			applied = append(applied, scoped.sourcePath)
		}
		if len(cfgs) > 1 {
			fmt.Fprintf(os.Stderr, "applied %d config file(s)\n", len(applied))
		}
	case "validate":
		cfgs, err := loadConfigsWithFuncs(confPath, validateFuncs(), true)
		check(err)
		check(runValidate(cfgs))
	case "run":
		check(runRunSubcommand(ctx, out, confPath, dryRun, args[1:]))
	case "import-ecschedule":
		importEcschedule(args[1:])
	default:
		flag.Usage()
		os.Exit(exitErr)
	}
}

// stsAccountResolver fetches the calling account ID via sts:GetCallerIdentity.
// It's a package-level variable so tests can swap a fake without standing up
// a real AWS client. The default uses the SDK's region-default chain because
// GetCallerIdentity is a global call — the region argument is irrelevant.
var stsAccountResolver = func(ctx context.Context) (string, error) {
	awsCfg, err := loadAWS(ctx, "")
	if err != nil {
		return "", err
	}
	cli := sts.NewFromConfig(awsCfg)
	out, err := cli.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Account), nil
}

// autoResolveAccountEnv populates AWS_ACCOUNT_ID from sts:GetCallerIdentity
// when the env var is empty. Online subcommands (diff/apply/dump/run) call
// this before loadConfigs so configs that omit a top-level `account:` (the
// common ecschedule shape) still resolve ARN shorthand correctly.
//
// On STS failure we leave the env var unset and continue silently:
// expandShortcuts surfaces a clear "shorthand requires account" error if the
// value is actually needed, and the upcoming preflightCheck will report any
// real credential problem with full context. Calling it from validate /
// import-ecschedule would defeat the offline guarantee, so they don't.
func autoResolveAccountEnv(ctx context.Context) {
	if os.Getenv("AWS_ACCOUNT_ID") != "" {
		return
	}
	id, err := stsAccountResolver(ctx)
	if err != nil || id == "" {
		return
	}
	_ = os.Setenv("AWS_ACCOUNT_ID", id)
}

// preflightCheck verifies AWS credentials by calling sts:GetCallerIdentity
// once per region present in cfgs (deduplicated). It runs before any
// mutation so an apply doesn't get half-way then trip on expired SSO.
// Errors here are surfaced with the AWS error wrapped so the user sees
// the underlying cause directly.
func preflightCheck(ctx context.Context, cfgs []*Config) error {
	seen := map[string]bool{}
	regions := []string{}
	for _, c := range cfgs {
		if seen[c.Region] {
			continue
		}
		seen[c.Region] = true
		regions = append(regions, c.Region)
	}
	for _, region := range regions {
		awsCfg, err := loadAWS(ctx, region)
		if err != nil {
			return fmt.Errorf("AWS credentials (region=%q): %w", region, err)
		}
		cli := sts.NewFromConfig(awsCfg)
		if _, err := cli.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
			return fmt.Errorf("sts:GetCallerIdentity (region=%q): %w", region, err)
		}
	}
	return nil
}

// applySummary prints a one-line summary to stderr when a multi-file apply
// fails partway through. With a single config or zero applied so far there's
// nothing useful to say, so it stays silent.
func applySummary(applied []string, cfgs []*Config, failedIdx int, err error) {
	if len(cfgs) <= 1 || len(applied) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "applied %d of %d config file(s) before error in %s\n",
		len(applied), len(cfgs), cfgs[failedIdx].sourcePath)
}

// stdinIsTTY reports whether stdin is attached to a real terminal. Non-TTY
// (CI / piped / `< /dev/null`) skips the interactive apply confirmation by
// default. Uses golang.org/x/term so /dev/null isn't misclassified as a
// terminal (it is a character device).
func stdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// confirmApply prompts on prompt and reads a line from in. Only the literal
// "yes" (after trim) confirms; everything else aborts. Stricter than y/Y to
// guard against accidental confirmations on dangerous applies.
func confirmApply(prompt io.Writer, in io.Reader) bool {
	fmt.Fprint(prompt, "ebschedule will modify AWS resources. Run `diff` first to preview.\nApply changes? Type 'yes' to continue: ")
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return false
	}
	return strings.TrimSpace(scanner.Text()) == "yes"
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// runDump always emits a single Config with both rules: and schedules:.
// Bus/group hints are taken from the first matching config file if present.
//
// If confPath does not exist (the bootstrap case), runDump falls through
// using AWS defaults. Other load errors (parse / template / strict YAML)
// are surfaced so typos can't be silently swallowed.
//
// tagFilter, when non-empty, restricts the dumped Rules to those carrying
// every listed tag; useful for bootstrapping a config out of an account
// that holds rules from multiple stacks (e.g. -tag Service=my-app). The
// filter does not currently apply to Schedules (those scope is already
// per-group, a config-level decision).
func runDump(ctx context.Context, out io.Writer, confPath, prefix string, tagFilter map[string]string) error {
	region, bus, group := "", "default", "default"
	cfgs, err := loadConfigs(confPath)
	if err != nil && !errors.Is(err, errNoConfigFiles) {
		return err
	}
	if err == nil && len(cfgs) > 0 {
		region = cfgs[0].Region
		if cfgs[0].EventBusName != "" {
			bus = cfgs[0].EventBusName
		}
		if cfgs[0].GroupName != "" {
			group = cfgs[0].GroupName
		}
	}
	dumped := &Config{Region: region}
	if bus != "default" {
		dumped.EventBusName = bus
	}
	if group != "default" {
		dumped.GroupName = group
	}
	rules, err := dumpRulesFiltered(ctx, region, bus, prefix, tagFilter)
	if err != nil {
		return fmt.Errorf("dump rules: %w", err)
	}
	dumped.Rules = rules
	schedules, err := dumpSchedules(ctx, region, group, prefix)
	if err != nil {
		return fmt.Errorf("dump schedules: %w", err)
	}
	dumped.Schedules = schedules
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	return enc.Encode(dumped)
}

// --- file expansion (template + glob) --------------------------------------

type expandedFile struct {
	path string
	data []byte
}

func expandFiles(pattern string, funcs template.FuncMap, jsonnetOffline bool) ([]expandedFile, error) {
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		if _, err := os.Stat(pattern); err != nil {
			return nil, fmt.Errorf("%w: %s", errNoConfigFiles, pattern)
		}
		paths = []string{pattern}
	}
	sort.Strings(paths)
	out := make([]expandedFile, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var exp []byte
		if isJsonnet(p) {
			// .jsonnet files run through go-jsonnet directly. Skipping
			// text/template avoids surprising double substitution; jsonnet
			// has its own std.extVar / std.env machinery.
			exp, err = evalJsonnet(p, raw, jsonnetOffline)
		} else {
			exp, err = expandTemplate(raw, funcs)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, expandedFile{path: p, data: exp})
	}
	return out, nil
}

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
		helper := newSSMHelper(ctx)
		vm.NativeFunction(jsonnetSsmFunc(helper))
		vm.NativeFunction(jsonnetSsmListFunc(helper))
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
func jsonnetSsmFunc(h *ssmHelper) *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   "ssm",
		Params: []ast.Identifier{"name"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("ssm: name must be a non-empty string")
			}
			return h.get(name)
		},
	}
}

// jsonnetSsmListFunc registers `ssmList(name)`: SSM Parameter Store value
// split on commas, returned as a jsonnet array. Cleaner than indexing
// against a CSV string; for one-off element access write
// `std.native('ssmList')(name)[idx]`. A non-StringList parameter comes
// back as a one-element array, which keeps caller iteration uniform.
func jsonnetSsmListFunc(h *ssmHelper) *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   "ssmList",
		Params: []ast.Identifier{"name"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("ssmList: name must be a non-empty string")
			}
			parts, err := h.list(name)
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

func tfstateStub(name string, params []ast.Identifier) *jsonnet.NativeFunction {
	return &jsonnet.NativeFunction{
		Name:   name,
		Params: params,
		Func: func([]any) (any, error) {
			return nil, fmt.Errorf("%s template func used but %s is not set", name, envTfstateURL)
		},
	}
}

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

func loadConfigs(pattern string) ([]*Config, error) {
	return loadConfigsWithFuncs(pattern, runtimeFuncs(), false)
}

func loadConfigsWithFuncs(pattern string, funcs template.FuncMap, jsonnetOffline bool) ([]*Config, error) {
	files, err := expandFiles(pattern, funcs, jsonnetOffline)
	if err != nil {
		return nil, err
	}
	out := make([]*Config, 0, len(files))
	for _, f := range files {
		// Empty file (e.g. -conf /dev/null) is treated as "no config" rather
		// than a parse error: lets `dump` run against AWS defaults without
		// implicitly picking up ebschedule.yaml in cwd.
		if len(bytes.TrimSpace(f.data)) == 0 {
			continue
		}
		var c Config
		dec := yaml.NewDecoder(bytes.NewReader(f.data))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", f.path, err)
		}
		c.sourcePath = f.path
		if c.Rules == nil && c.Schedules == nil {
			return nil, fmt.Errorf("%s: neither rules: nor schedules: present", f.path)
		}
		if err := resolveBaseFile(&c, funcs, map[string]bool{}); err != nil {
			return nil, fmt.Errorf("%s: %w", f.path, err)
		}
		// Fail-fast on bad cron at load time, so apply / diff don't get
		// part-way through before AWS rejects the rule. validate runs the
		// same check (plus the rest), so it's still the right tool for
		// surfacing every error at once; this short-circuits typos.
		if err := preflightCronExpressions(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", f.path, err)
		}
		out = append(out, &c)
	}
	return out, nil
}

// preflightCronExpressions parses every Rule.scheduleExpression and
// Schedule.scheduleExpression with the same validator validate uses, so
// a malformed cron (e.g. out-of-range minute, missing year field) errors
// during load instead of midway through apply. Stops on the first
// error, with a path so the user can locate the offending entry.
func preflightCronExpressions(c *Config) error {
	for _, r := range c.Rules {
		if r.ScheduleExpression == "" {
			continue
		}
		if err := validateScheduleExpression(r.ScheduleExpression); err != nil {
			return fmt.Errorf("rules[%s]: %w", r.Name, err)
		}
	}
	for _, s := range c.Schedules {
		if err := validateScheduleExpression(s.ScheduleExpression); err != nil {
			return fmt.Errorf("schedules[%s]: %w", s.Name, err)
		}
	}
	return nil
}

// resolveBaseFile reads BaseFile (if set) and inherits scalar fields the
// child didn't set, plus merges Tags (child overrides on conflict). The
// parent may itself declare a baseFile; recursion is bounded by `visited`
// (keyed on absolute path) so cycles fail fast.
//
// Rules and Schedules are NEVER inherited - each file owns its own
// resources, baseFile only shares the scaffolding (region / account /
// cluster / groupName / eventBusName / trackingId / tags).
func resolveBaseFile(c *Config, funcs template.FuncMap, visited map[string]bool) error {
	if c.BaseFile == "" {
		return nil
	}
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(c.sourcePath), c.BaseFile))
	if err != nil {
		return fmt.Errorf("baseFile: %w", err)
	}
	if visited[abs] {
		return fmt.Errorf("baseFile cycle detected at %s", abs)
	}
	visited[abs] = true

	raw, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("baseFile %s: %w", abs, err)
	}
	expanded, err := expandTemplate(raw, funcs)
	if err != nil {
		return fmt.Errorf("baseFile %s: %w", abs, err)
	}
	var base Config
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&base); err != nil {
		return fmt.Errorf("baseFile %s: %w", abs, err)
	}
	base.sourcePath = abs
	// A baseFile is for shared scaffolding; declaring rules / schedules
	// inside it is almost certainly a mistake (they would otherwise be
	// silently ignored). Refuse rather than fail open.
	if len(base.Rules) > 0 || len(base.Schedules) > 0 {
		return fmt.Errorf("baseFile %s: must not declare rules: or schedules: (those belong in the child file)", abs)
	}

	// Recurse: parent may declare its own baseFile.
	if err := resolveBaseFile(&base, funcs, visited); err != nil {
		return err
	}

	// Inherit scalars the child left empty.
	if c.Region == "" {
		c.Region = base.Region
	}
	if c.GroupName == "" {
		c.GroupName = base.GroupName
	}
	if c.EventBusName == "" {
		c.EventBusName = base.EventBusName
	}
	if c.TrackingID == "" {
		c.TrackingID = base.TrackingID
	}
	// Merge tags: parent provides defaults, child overrides on conflict.
	if len(base.Tags) > 0 || len(c.Tags) > 0 {
		merged := map[string]string{}
		maps.Copy(merged, base.Tags)
		maps.Copy(merged, c.Tags)
		c.Tags = merged
	}
	return nil
}

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

func loadAWS(ctx context.Context, region string) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// --- diff helpers ----------------------------------------------------------

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

func mergeTags(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := map[string]string{}
	maps.Copy(out, base)
	maps.Copy(out, override)
	return out
}

// --- tag reconciliation ----------------------------------------------------

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

// --- time helpers ----------------------------------------------------------

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

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

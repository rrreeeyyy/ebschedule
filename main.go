// ebschedule: a small declarative CLI for managing Amazon EventBridge Rules
// and EventBridge Scheduler Schedules from a single YAML (or jsonnet)
// config file.
//
//	ebschedule [-conf FILE_OR_GLOB] [-dry-run] [-prune] [-auto-approve] [-target KIND:NAME]... <dump|diff|apply|validate> [name-prefix]
//	ebschedule [-conf FILE_OR_GLOB] [-dry-run] run -rule NAME
//	ebschedule import-ecschedule [-in FILE] [-account NUM] [-region REGION] [-tracking-id ID]
//
// Config files run through text/template (YAML) or go-jsonnet (.jsonnet /
// .libsonnet) before parsing. Available helpers:
//
//	env / must_env / ssm / tfstate / tfstatef
package main

import (
	"bufio"
	"bytes"
	"context"
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

	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

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
		os.Exit(exitErr)
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

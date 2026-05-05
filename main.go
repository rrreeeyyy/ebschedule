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
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/pmezard/go-difflib/difflib"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// trackingTagKey marks resources managed by ebschedule. -prune only deletes
// resources carrying this tag with the trackingId from the same config.
const trackingTagKey = "ebschedule-tracking-id"

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
	Rules        []*Rule           `yaml:"rules,omitempty"`
	Schedules    []*Schedule       `yaml:"schedules,omitempty"`

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

// --- main ------------------------------------------------------------------

func main() {
	var (
		confPath       string
		dryRun         bool
		prune          bool
		showVersion    bool
		dumpTrackingID string
		autoApprove    bool
	)
	flag.StringVar(&confPath, "conf", "ebschedule.yaml", "config file or glob (e.g. 'config/*.yaml')")
	flag.BoolVar(&dryRun, "dry-run", false, "do not actually apply")
	flag.BoolVar(&prune, "prune", false, "delete tracked resources absent from config")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.StringVar(&dumpTrackingID, "tracking-id", "", "(dump) only emit Rules tagged ebschedule-tracking-id=<ID>")
	flag.BoolVar(&autoApprove, "auto-approve", false, "(apply) skip the interactive confirmation prompt")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage:
  ebschedule [-conf FILE_OR_GLOB] [-dry-run] [-prune] [-auto-approve] <dump|diff|apply|validate> [name-prefix]
  ebschedule [-conf FILE_OR_GLOB] [-tracking-id ID] dump [name-prefix]
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
		check(runDump(ctx, out, confPath, prefix, dumpTrackingID))
	case "diff":
		cfgs, err := loadConfigs(confPath)
		check(err)
		drift := false
		for _, cfg := range cfgs {
			if len(cfgs) > 1 {
				fmt.Fprintf(out, "# %s\n", cfg.sourcePath)
			}
			if cfg.Rules != nil {
				d, err := diffRules(ctx, out, cfg)
				check(err)
				drift = drift || d
			}
			if cfg.Schedules != nil {
				d, err := diffSchedules(ctx, out, cfg)
				check(err)
				drift = drift || d
			}
		}
		if drift {
			os.Exit(exitDrift)
		}
	case "apply":
		cfgs, err := loadConfigs(confPath)
		check(err)
		if !dryRun && !autoApprove && stdinIsTTY() {
			if !confirmApply(os.Stderr, os.Stdin) {
				fmt.Fprintln(os.Stderr, "aborted")
				os.Exit(exitErr)
			}
		}
		for _, cfg := range cfgs {
			if len(cfgs) > 1 {
				fmt.Fprintf(out, "# %s\n", cfg.sourcePath)
			}
			if cfg.Rules != nil {
				check(applyRules(ctx, out, cfg, dryRun, prune))
			}
			if cfg.Schedules != nil {
				check(applySchedules(ctx, out, cfg, dryRun, prune))
			}
		}
	case "validate":
		cfgs, err := loadConfigsWithFuncs(confPath, validateFuncs())
		check(err)
		check(runValidate(cfgs))
	case "import-ecschedule":
		importEcschedule(args[1:])
	default:
		flag.Usage()
		os.Exit(exitErr)
	}
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
// trackingIDFilter, when non-empty, restricts the dumped Rules to those
// tagged with ebschedule-tracking-id=<value>; useful for dumping only
// resources you already manage out of an account that holds many. The
// filter does not currently apply to Schedules (those are scoped per
// schedule-group, which is already a config-level decision).
func runDump(ctx context.Context, out io.Writer, confPath, prefix, trackingIDFilter string) error {
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
	rules, err := dumpRulesFiltered(ctx, region, bus, prefix, trackingIDFilter)
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

func expandFiles(pattern string, funcs template.FuncMap) ([]expandedFile, error) {
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
		exp, err := expandTemplate(raw, funcs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, expandedFile{path: p, data: exp})
	}
	return out, nil
}

func loadConfigs(pattern string) ([]*Config, error) {
	return loadConfigsWithFuncs(pattern, runtimeFuncs())
}

func loadConfigsWithFuncs(pattern string, funcs template.FuncMap) ([]*Config, error) {
	files, err := expandFiles(pattern, funcs)
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
		out = append(out, &c)
	}
	return out, nil
}

// runtimeFuncs returns the FuncMap used by dump/diff/apply. Hits AWS for SSM
// and errors loudly if must_env is unset. Each call returns a fresh closure
// that owns its own lazily-constructed *ssm.Client, so test runs don't share
// state and there's no package-level singleton.
func runtimeFuncs() template.FuncMap {
	var client *ssm.Client
	ssmFetch := func(name string) (string, error) {
		if client == nil {
			cfg, err := awsconfig.LoadDefaultConfig(context.Background())
			if err != nil {
				return "", err
			}
			client = ssm.NewFromConfig(cfg)
		}
		out, err := client.GetParameter(context.Background(), &ssm.GetParameterInput{
			Name: aws.String(name), WithDecryption: aws.Bool(true),
		})
		if err != nil {
			return "", fmt.Errorf("ssm:%s: %w", name, err)
		}
		return aws.ToString(out.Parameter.Value), nil
	}
	return template.FuncMap{
		"env": os.Getenv,
		"must_env": func(name string) (string, error) {
			v := os.Getenv(name)
			if v == "" {
				return "", fmt.Errorf("env var %s is not set", name)
			}
			return v, nil
		},
		"ssm": ssmFetch,
	}
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
		"ssm": func(name string) string { return "<ssm:" + name + ">" },
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
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
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
	for k, v := range desired {
		full[k] = v
	}
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

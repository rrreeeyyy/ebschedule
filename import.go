package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// --- ecschedule YAML schema (subset we care about) ------------------------

type ecsConfig struct {
	Region   string `yaml:"region"`
	Cluster  string `yaml:"cluster"`
	Account  string `yaml:"account,omitempty"`  // some forks
	BaseFile string `yaml:"baseFile,omitempty"` // include sibling file
	// Role at the top level is ecschedule's default IAM role for any
	// rule that doesn't specify one. We chain it into target.RoleArn
	// during conversion so an imported config has the same role
	// resolution ecschedule applied at runtime.
	Role string `yaml:"role,omitempty"`
	// Plugins is ecschedule's per-config plugin block (most commonly
	// the tfstate-lookup plugin). ebschedule wires tfstate via the
	// EBSCHEDULE_TFSTATE_URL env var instead, so we capture the block
	// only to surface a warning rather than silently drop user intent.
	Plugins []ecsPlugin `yaml:"plugins,omitempty"`
	Rules   []*ecsRule  `yaml:"rules"`
}

// ecsPlugin captures just enough of ecschedule's plugin entry to
// recognize the block on import. We do not execute plugins.
type ecsPlugin struct {
	Name   string         `yaml:"name"`
	Config map[string]any `yaml:"config,omitempty"`
}

type ecsRule struct {
	Name               string     `yaml:"name"`
	Description        string     `yaml:"description,omitempty"`
	ScheduleExpression string     `yaml:"scheduleExpression"`
	Disabled           bool       `yaml:"disabled,omitempty"`
	TargetID           string     `yaml:"targetId,omitempty"`
	Target             *ecsTarget `yaml:"target"`
}

type ecsTarget struct {
	TaskDefinition           string                            `yaml:"taskDefinition"`
	TaskCount                int32                             `yaml:"taskCount,omitempty"`
	LaunchType               string                            `yaml:"launchType,omitempty"`
	PlatformVersion          string                            `yaml:"platformVersion,omitempty"`
	PropagateTags            string                            `yaml:"propagateTags,omitempty"`
	Group                    string                            `yaml:"group,omitempty"`
	Role                     string                            `yaml:"role,omitempty"`
	NetworkConfiguration     *ecsNetworkConfig                 `yaml:"networkConfiguration,omitempty"`
	ContainerOverrides       []ecsContainerOverride            `yaml:"containerOverrides,omitempty"`
	TaskOverride             *ecsTaskOverride                  `yaml:"taskOverride,omitempty"`
	CapacityProviderStrategy []ecsCapacityProviderStrategyItem `yaml:"capacityProviderStrategy,omitempty"`
}

// ecsTaskOverride mirrors ecschedule's task-level cpu/memory override.
// AWS expects strings here (vCPU shorthand like "0.5" or unit-stripped
// MiB like "1024"), so keep them as strings rather than int.
type ecsTaskOverride struct {
	Cpu    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}

// ecsCapacityProviderStrategyItem mirrors ecschedule's flat YAML shape.
type ecsCapacityProviderStrategyItem struct {
	CapacityProvider string `yaml:"capacityProvider"`
	Base             int32  `yaml:"base,omitempty"`
	Weight           int32  `yaml:"weight,omitempty"`
}

type ecsNetworkConfig struct {
	AwsvpcConfiguration *ecsAwsvpcConfig `yaml:"awsvpcConfiguration,omitempty"`
}

type ecsAwsvpcConfig struct {
	Subnets        []string `yaml:"subnets"`
	SecurityGroups []string `yaml:"securityGroups,omitempty"`
	AssignPublicIp string   `yaml:"assignPublicIp,omitempty"`
}

type ecsContainerOverride struct {
	Name              string      `yaml:"name"`
	Command           []string    `yaml:"command,omitempty"`
	Environment       []ecsEnvVar `yaml:"environment,omitempty"`
	Cpu               *int32      `yaml:"cpu,omitempty"`
	Memory            *int32      `yaml:"memory,omitempty"`
	MemoryReservation *int32      `yaml:"memoryReservation,omitempty"`
}

type ecsEnvVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// defaultECSEventsRole matches ecschedule's `defaultRole` constant.
// When neither the per-target role nor the top-level role is set, the
// importer falls back here so an imported config still has a roleArn
// to apply against — same chain ecschedule applies at runtime.
const defaultECSEventsRole = "ecsEventsRole"

// --- entrypoint ------------------------------------------------------------

func importEcschedule(args []string) {
	fs := flag.NewFlagSet("ebschedule import-ecschedule", flag.ExitOnError)
	in := fs.String("in", "", "input ecschedule YAML (default: stdin)")
	account := fs.String("account", "", `account ID; default: AWS_ACCOUNT_ID env, else "{{ must_env ... }}" placeholder`)
	region := fs.String("region", "", "override region")
	trackingID := fs.String("tracking-id", "imported", "trackingId for the new ebschedule config")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ebschedule import-ecschedule [-in FILE] [-account NUM] [-region REGION] [-tracking-id ID]")
		fs.PrintDefaults()
	}
	// flag.ExitOnError makes Parse os.Exit on bad input, so the err return
	// is always nil here in practice. Keep the explicit `_ =` to avoid
	// errcheck noise without pretending we'd handle it.
	_ = fs.Parse(args) //nolint:errcheck

	var data []byte
	var err error
	if *in == "" || *in == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*in)
	}
	check(err)

	var src ecsConfig
	check(yaml.Unmarshal(data, &src))

	// Resolve baseFile (region/cluster/account fallback).
	if src.BaseFile != "" {
		basePath := src.BaseFile
		if *in != "" {
			basePath = filepath.Join(filepath.Dir(*in), src.BaseFile)
		}
		baseData, err := os.ReadFile(basePath)
		check(err)
		var base ecsConfig
		check(yaml.Unmarshal(baseData, &base))
		if src.Region == "" {
			src.Region = base.Region
		}
		if src.Cluster == "" {
			src.Cluster = base.Cluster
		}
		if src.Account == "" {
			src.Account = base.Account
		}
	}

	if *region != "" {
		src.Region = *region
	}
	if src.Region == "" {
		check(fmt.Errorf("region is required (in input or via -region)"))
	}
	if src.Cluster == "" {
		check(fmt.Errorf("cluster is required (in input or baseFile)"))
	}

	acc := *account
	if acc == "" {
		acc = os.Getenv("AWS_ACCOUNT_ID")
	}
	if acc == "" {
		acc = src.Account
	}
	if acc == "" {
		acc = `{{ must_env "AWS_ACCOUNT_ID" }}`
	}

	out := convertEcschedule(&src, acc, *trackingID)

	enc := yaml.NewEncoder(os.Stdout)
	enc.SetIndent(2)
	check(enc.Encode(out))
}

// --- conversion ------------------------------------------------------------

func convertEcschedule(src *ecsConfig, account, trackingID string) *Config {
	out := &Config{
		Region:     src.Region,
		TrackingID: trackingID,
		Tags: map[string]string{
			"ManagedBy": "ebschedule",
		},
	}

	if len(src.Plugins) > 0 {
		names := make([]string, 0, len(src.Plugins))
		for _, p := range src.Plugins {
			names = append(names, p.Name)
		}
		fmt.Fprintf(os.Stderr,
			"warn: dropping plugins block (%s); ebschedule reads tfstate via the EBSCHEDULE_TFSTATE_URL env var instead — set that before running apply/diff.\n",
			strings.Join(names, ","))
	}

	for _, r := range src.Rules {
		rule := &Rule{
			Name:               r.Name,
			Description:        r.Description,
			ScheduleExpression: r.ScheduleExpression,
			State:              "ENABLED",
		}
		if r.Disabled {
			rule.State = "DISABLED"
		}

		if r.Target == nil {
			fmt.Fprintf(os.Stderr, "warn: rule %s: no target, skipping\n", r.Name)
			out.Rules = append(out.Rules, rule)
			continue
		}

		tid := r.TargetID
		if tid == "" {
			tid = "ecs"
		}

		// Role resolution chain matches ecschedule: per-target role
		// wins, falling back to the top-level role, falling back to
		// "ecsEventsRole". The constant default is what ecschedule
		// applies at apply time when neither yaml field is set, so
		// imports keep the same effective role rather than emitting an
		// empty roleArn that would trip apply later.
		role := r.Target.Role
		if role == "" {
			role = src.Role
		}
		if role == "" {
			role = defaultECSEventsRole
		}
		target := &Target{
			ID:      tid,
			Arn:     fmt.Sprintf("arn:aws:ecs:%s:%s:cluster/%s", src.Region, account, src.Cluster),
			RoleArn: resolveRoleArn(role, account),
		}

		ep := &RuleEcsParameters{
			TaskDefinitionArn: resolveTaskDefArn(r.Target.TaskDefinition, src.Region, account),
			TaskCount:         r.Target.TaskCount,
			LaunchType:        r.Target.LaunchType,
			PlatformVersion:   r.Target.PlatformVersion,
			Group:             r.Target.Group,
			PropagateTags:     r.Target.PropagateTags,
		}
		if nc := r.Target.NetworkConfiguration; nc != nil && nc.AwsvpcConfiguration != nil {
			ep.Subnets = nc.AwsvpcConfiguration.Subnets
			ep.SecurityGroups = nc.AwsvpcConfiguration.SecurityGroups
			ep.AssignPublicIp = nc.AwsvpcConfiguration.AssignPublicIp
		}
		for _, c := range r.Target.CapacityProviderStrategy {
			ep.CapacityProviderStrategy = append(ep.CapacityProviderStrategy, CapacityProviderStrategyItem(c))
		}
		target.EcsParameters = ep

		if len(r.Target.ContainerOverrides) > 0 || r.Target.TaskOverride != nil {
			target.Input = jsonField(buildContainerOverridesInput(r.Target.ContainerOverrides, r.Target.TaskOverride))
		}

		rule.Targets = []*Target{target}
		out.Rules = append(out.Rules, rule)
	}
	return out
}

func resolveRoleArn(role, account string) string {
	if role == "" {
		return ""
	}
	if strings.HasPrefix(role, "arn:") {
		return role
	}
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", account, role)
}

func resolveTaskDefArn(td, region, account string) string {
	if td == "" {
		return ""
	}
	if strings.HasPrefix(td, "arn:") {
		return td
	}
	return fmt.Sprintf("arn:aws:ecs:%s:%s:task-definition/%s", region, account, td)
}

// buildContainerOverridesInput emits the JSON expected by EventBridge's
// `Input` field for an ECS RunTask target. Both containerOverrides and
// taskOverride live at the top level of the JSON (not nested under
// `taskOverride`) — that's the shape ecschedule emits and the one the
// EventBridge → ECS RunTask integration expects.
func buildContainerOverridesInput(overrides []ecsContainerOverride, taskOverride *ecsTaskOverride) string {
	type envKV struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	type containerOverride struct {
		Name              string   `json:"name"`
		Command           []string `json:"command,omitempty"`
		Environment       []envKV  `json:"environment,omitempty"`
		Cpu               *int32   `json:"cpu,omitempty"`
		Memory            *int32   `json:"memory,omitempty"`
		MemoryReservation *int32   `json:"memoryReservation,omitempty"`
	}
	type taskOverrideJSON struct {
		Cpu    string `json:"cpu,omitempty"`
		Memory string `json:"memory,omitempty"`
	}
	type wrapper struct {
		ContainerOverrides []containerOverride `json:"containerOverrides,omitempty"`
		TaskOverride       *taskOverrideJSON   `json:"taskOverride,omitempty"`
	}

	cs := make([]containerOverride, 0, len(overrides))
	for _, o := range overrides {
		co := containerOverride{
			Name:              o.Name,
			Command:           o.Command,
			Cpu:               o.Cpu,
			Memory:            o.Memory,
			MemoryReservation: o.MemoryReservation,
		}
		for _, e := range o.Environment {
			co.Environment = append(co.Environment, envKV(e))
		}
		cs = append(cs, co)
	}
	w := wrapper{ContainerOverrides: cs}
	if taskOverride != nil && (taskOverride.Cpu != "" || taskOverride.Memory != "") {
		w.TaskOverride = &taskOverrideJSON{Cpu: taskOverride.Cpu, Memory: taskOverride.Memory}
	}
	// json.Marshal cannot fail on a struct made of strings, slices, and
	// pointer-to-int — every type here implements json.Marshaler-equivalent
	// behavior trivially. Discard the err return to keep the call site
	// flat; if a future field addition breaks this assumption, the next
	// `validate` run on the converter's output will surface invalid JSON.
	b, _ := json.Marshal(w) //nolint:errcheck
	return string(b)
}

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
	Region   string     `yaml:"region"`
	Cluster  string     `yaml:"cluster"`
	Account  string     `yaml:"account,omitempty"`  // some forks
	BaseFile string     `yaml:"baseFile,omitempty"` // include sibling file
	Rules    []*ecsRule `yaml:"rules"`
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
	TaskDefinition       string                 `yaml:"taskDefinition"`
	TaskCount            int32                  `yaml:"taskCount,omitempty"`
	LaunchType           string                 `yaml:"launchType,omitempty"`
	PlatformVersion      string                 `yaml:"platformVersion,omitempty"`
	PropagateTags        string                 `yaml:"propagateTags,omitempty"`
	Group                string                 `yaml:"group,omitempty"`
	Role                 string                 `yaml:"role,omitempty"`
	NetworkConfiguration *ecsNetworkConfig      `yaml:"networkConfiguration,omitempty"`
	ContainerOverrides   []ecsContainerOverride `yaml:"containerOverrides,omitempty"`
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
	_ = fs.Parse(args)

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

		target := &Target{
			ID:      tid,
			Arn:     fmt.Sprintf("arn:aws:ecs:%s:%s:cluster/%s", src.Region, account, src.Cluster),
			RoleArn: resolveRoleArn(r.Target.Role, account),
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
		target.EcsParameters = ep

		if len(r.Target.ContainerOverrides) > 0 {
			target.Input = buildContainerOverridesInput(r.Target.ContainerOverrides)
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
// `Input` field for an ECS RunTask target.
func buildContainerOverridesInput(overrides []ecsContainerOverride) string {
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
	type wrapper struct {
		ContainerOverrides []containerOverride `json:"containerOverrides"`
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
			co.Environment = append(co.Environment, envKV{Name: e.Name, Value: e.Value})
		}
		cs = append(cs, co)
	}
	b, _ := json.MarshalIndent(wrapper{cs}, "", "  ")
	return string(b)
}

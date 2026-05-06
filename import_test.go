package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolveRoleArn(t *testing.T) {
	cases := []struct {
		name, role, account, want string
	}{
		{"empty", "", "1", ""},
		{"name expanded", "ecsEventsRole", "123456789012", "arn:aws:iam::123456789012:role/ecsEventsRole"},
		{"already arn", "arn:aws:iam::999:role/Custom", "1", "arn:aws:iam::999:role/Custom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRoleArn(tc.role, tc.account); got != tc.want {
				t.Errorf("resolveRoleArn(%q, %q) = %q, want %q", tc.role, tc.account, got, tc.want)
			}
		})
	}
}

func TestResolveTaskDefArn(t *testing.T) {
	cases := []struct {
		name, td, region, account, want string
	}{
		{"empty", "", "us-east-1", "1", ""},
		{"family", "my-task", "ap-northeast-1", "1", "arn:aws:ecs:ap-northeast-1:1:task-definition/my-task"},
		{"family:rev", "my-task:42", "ap-northeast-1", "1", "arn:aws:ecs:ap-northeast-1:1:task-definition/my-task:42"},
		{"already arn", "arn:aws:ecs:us-east-1:9:task-definition/x:1", "ap-northeast-1", "1", "arn:aws:ecs:us-east-1:9:task-definition/x:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveTaskDefArn(tc.td, tc.region, tc.account); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestConvertEcschedule(t *testing.T) {
	src := &ecsConfig{
		Region:  "ap-northeast-1",
		Cluster: "my-cluster",
		Rules: []*ecsRule{
			{
				Name:               "nightly",
				Description:        "ETL",
				ScheduleExpression: "cron(0 18 * * ? *)",
				Disabled:           false,
				Target: &ecsTarget{
					TaskDefinition:  "my-batch",
					LaunchType:      "FARGATE",
					PlatformVersion: "LATEST",
					Role:            "ecsEventsRole",
					NetworkConfiguration: &ecsNetworkConfig{
						AwsvpcConfiguration: &ecsAwsvpcConfig{
							Subnets:        []string{"subnet-a"},
							AssignPublicIp: "DISABLED",
						},
					},
					ContainerOverrides: []ecsContainerOverride{
						{
							Name:    "app",
							Command: []string{"echo", "hi"},
							Environment: []ecsEnvVar{
								{Name: "K", Value: "V"},
							},
						},
					},
				},
			},
			{
				Name:               "hourly",
				ScheduleExpression: "rate(1 hour)",
				Disabled:           true,
				TargetID:           "custom-id",
				Target:             &ecsTarget{TaskDefinition: "my-sync", Role: "arn:aws:iam::9:role/Custom"},
			},
			{
				Name:               "no-target",
				ScheduleExpression: "rate(2 hours)",
				// Target intentionally nil to exercise the warn-and-skip-target path.
			},
		},
	}
	out := convertEcschedule(src, "111122223333", "my-tracking")

	if out.Region != "ap-northeast-1" {
		t.Errorf("region = %q", out.Region)
	}
	if out.TrackingID != "my-tracking" {
		t.Errorf("trackingID = %q", out.TrackingID)
	}
	if out.Tags["ManagedBy"] != "ebschedule" {
		t.Errorf("ManagedBy tag missing: %v", out.Tags)
	}
	if len(out.Rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(out.Rules))
	}

	t.Run("nightly", func(t *testing.T) {
		r := out.Rules[0]
		if r.State != "ENABLED" {
			t.Errorf("state = %q", r.State)
		}
		if len(r.Targets) != 1 {
			t.Fatal("expected 1 target")
		}
		tgt := r.Targets[0]
		if tgt.ID != "ecs" {
			t.Errorf("default target id = %q, want ecs", tgt.ID)
		}
		if tgt.Arn != "arn:aws:ecs:ap-northeast-1:111122223333:cluster/my-cluster" {
			t.Errorf("cluster arn = %q", tgt.Arn)
		}
		if tgt.RoleArn != "arn:aws:iam::111122223333:role/ecsEventsRole" {
			t.Errorf("role arn = %q", tgt.RoleArn)
		}
		if tgt.EcsParameters.TaskDefinitionArn != "arn:aws:ecs:ap-northeast-1:111122223333:task-definition/my-batch" {
			t.Errorf("taskDef arn = %q", tgt.EcsParameters.TaskDefinitionArn)
		}
		if tgt.EcsParameters.AssignPublicIp != "DISABLED" {
			t.Errorf("AssignPublicIp = %q", tgt.EcsParameters.AssignPublicIp)
		}
		if !strings.Contains(string(tgt.Input), "containerOverrides") {
			t.Errorf("Input missing containerOverrides: %s", tgt.Input)
		}
		// Input must be valid JSON.
		var probe any
		if err := json.Unmarshal([]byte(tgt.Input), &probe); err != nil {
			t.Errorf("Input not valid JSON: %v", err)
		}
	})

	t.Run("hourly: disabled, custom targetId, role-arn passthrough", func(t *testing.T) {
		r := out.Rules[1]
		if r.State != "DISABLED" {
			t.Errorf("state = %q, want DISABLED", r.State)
		}
		tgt := r.Targets[0]
		if tgt.ID != "custom-id" {
			t.Errorf("targetId override = %q", tgt.ID)
		}
		if tgt.RoleArn != "arn:aws:iam::9:role/Custom" {
			t.Errorf("role arn passthrough failed: %q", tgt.RoleArn)
		}
	})

	t.Run("no-target: rule kept but no targets emitted", func(t *testing.T) {
		r := out.Rules[2]
		if len(r.Targets) != 0 {
			t.Errorf("expected 0 targets, got %d", len(r.Targets))
		}
	})
}

func TestBuildContainerOverridesInput(t *testing.T) {
	cpu := int32(256)
	overrides := []ecsContainerOverride{
		{
			Name:    "app",
			Command: []string{"go", "test"},
			Environment: []ecsEnvVar{
				{Name: "DEBUG", Value: "true"},
			},
			Cpu: &cpu,
		},
	}
	out := buildContainerOverridesInput(overrides, nil)

	var parsed struct {
		ContainerOverrides []struct {
			Name        string   `json:"name"`
			Command     []string `json:"command"`
			Environment []struct {
				Name, Value string
			} `json:"environment"`
			Cpu *int32 `json:"cpu"`
		} `json:"containerOverrides"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(parsed.ContainerOverrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(parsed.ContainerOverrides))
	}
	co := parsed.ContainerOverrides[0]
	if co.Name != "app" || co.Cpu == nil || *co.Cpu != 256 {
		t.Errorf("override mismatch: %+v", co)
	}
	if len(co.Environment) != 1 || co.Environment[0].Name != "DEBUG" {
		t.Errorf("env mismatch: %+v", co.Environment)
	}
}

func TestBuildContainerOverridesInputWithTaskOverride(t *testing.T) {
	to := &ecsTaskOverride{Cpu: "512", Memory: "1024"}
	out := buildContainerOverridesInput(nil, to)
	var parsed struct {
		ContainerOverrides []any `json:"containerOverrides"`
		TaskOverride       *struct {
			Cpu, Memory string
		} `json:"taskOverride"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(parsed.ContainerOverrides) != 0 {
		t.Errorf("containerOverrides should be omitted when nil, got %v", parsed.ContainerOverrides)
	}
	if parsed.TaskOverride == nil || parsed.TaskOverride.Cpu != "512" || parsed.TaskOverride.Memory != "1024" {
		t.Errorf("taskOverride mismatch: %+v", parsed.TaskOverride)
	}
}

func TestConvertEcscheduleCapturesTaskOverride(t *testing.T) {
	src := []byte(`region: ap-northeast-1
cluster: my-cluster
rules:
  - name: nightly
    scheduleExpression: cron(0 18 * * ? *)
    target:
      taskDefinition: app:1
      taskOverride:
        cpu: "512"
        memory: "1024"
      containerOverrides:
        - name: app
          command: [migrate]
`)
	var parsed ecsConfig
	if err := yaml.Unmarshal(src, &parsed); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	cfg := convertEcschedule(&parsed, "111111111111", "test")
	if len(cfg.Rules) != 1 || len(cfg.Rules[0].Targets) != 1 {
		t.Fatalf("unexpected shape: %+v", cfg)
	}
	in := string(cfg.Rules[0].Targets[0].Input)
	if !strings.Contains(in, `"taskOverride":{"cpu":"512","memory":"1024"}`) {
		t.Errorf("taskOverride missing from input: %s", in)
	}
	if !strings.Contains(in, `"containerOverrides":[{"name":"app"`) {
		t.Errorf("containerOverrides missing from input: %s", in)
	}
}

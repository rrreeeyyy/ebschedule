package main

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schtypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
)

func fullScheduleTarget() *ScheduleTarget {
	return &ScheduleTarget{
		Arn:     "arn:aws:ecs:us-east-1:1:cluster/c",
		RoleArn: "arn:aws:iam::1:role/r",
		Input:   `{"k":"v"}`,
		DeadLetterConfig: &DeadLetterConfig{
			Arn: "arn:aws:sqs:us-east-1:1:q",
		},
		RetryPolicy: &RetryPolicy{
			MaximumRetryAttempts:     2,
			MaximumEventAgeInSeconds: 600,
		},
		EcsParameters: &SchedEcsParameters{
			TaskDefinitionArn: "arn:aws:ecs:us-east-1:1:task-definition/x:1",
			TaskCount:         1,
			LaunchType:        "",
			PlatformVersion:   "LATEST",
			Subnets:           []string{"subnet-a"},
			SecurityGroups:    []string{"sg-1"},
			AssignPublicIp:    "DISABLED",
			Group:             "g",
			PropagateTags:     "TASK_DEFINITION",
			CapacityProviderStrategy: []CapacityProviderStrategyItem{
				{CapacityProvider: "FARGATE_SPOT", Weight: 4},
			},
			EnableECSManagedTags: true,
			EnableExecuteCommand: true,
			PlacementConstraints: []PlacementConstraint{
				{Type: "distinctInstance"},
			},
			PlacementStrategy: []PlacementStrategy{
				{Type: "binpack", Field: "memory"},
			},
			ReferenceID: "ref-sched",
			Tags: []KeyValuePair{
				{Name: "Owner", Value: "team-b"},
			},
		},
		SqsParameters: &SqsParameters{
			MessageGroupId: "g1",
		},
		KinesisParameters: &SchedKinesisParameters{
			PartitionKey: "literal-key",
		},
		SageMakerPipelineParameters: &SageMakerPipelineParameters{
			PipelineParameterList: []SageMakerPipelineParameter{
				{Name: "p1", Value: "v1"},
			},
		},
		EventBridgeParameters: &EventBridgeParameters{
			DetailType: "MyEvent",
			Source:     "my.app",
		},
	}
}

func TestSchedTarget_RoundTrip(t *testing.T) {
	src := fullScheduleTarget()
	at, err := toAWSSchedTarget(src)
	if err != nil {
		t.Fatal(err)
	}
	got := fromRemoteSchedTarget(at)
	if !reflect.DeepEqual(got, src) {
		t.Errorf("round-trip mismatch\n got: %+v\nwant: %+v", got, src)
	}
}

func TestSchedTarget_MinimalRoundTrip(t *testing.T) {
	src := &ScheduleTarget{
		Arn:     "arn:aws:lambda:us-east-1:1:function:f",
		RoleArn: "arn:aws:iam::1:role/r",
	}
	at, err := toAWSSchedTarget(src)
	if err != nil {
		t.Fatal(err)
	}
	got := fromRemoteSchedTarget(at)
	if !reflect.DeepEqual(got, src) {
		t.Errorf("minimal round-trip\n got: %+v\nwant: %+v", got, src)
	}
}

func TestToAWSSchedTarget_NilSource(t *testing.T) {
	if _, err := toAWSSchedTarget(nil); err == nil {
		t.Error("expected error for nil ScheduleTarget")
	}
}

func TestToAWSSchedTarget_EcsNetworkConfigOnlyWhenSubnets(t *testing.T) {
	t.Run("no subnets -> no network config", func(t *testing.T) {
		at, err := toAWSSchedTarget(&ScheduleTarget{
			Arn: "arn:...", RoleArn: "arn:...",
			EcsParameters: &SchedEcsParameters{
				TaskDefinitionArn: "arn:...:task-definition/x",
				LaunchType:        "FARGATE",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if at.EcsParameters.NetworkConfiguration != nil {
			t.Errorf("NetworkConfiguration should be nil when no subnets")
		}
	})
	t.Run("with subnets -> attached", func(t *testing.T) {
		at, err := toAWSSchedTarget(&ScheduleTarget{
			Arn: "arn:...", RoleArn: "arn:...",
			EcsParameters: &SchedEcsParameters{
				TaskDefinitionArn: "arn:...:task-definition/x",
				Subnets:           []string{"s-1"},
				AssignPublicIp:    "ENABLED",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		nc := at.EcsParameters.NetworkConfiguration
		if nc == nil || nc.AwsvpcConfiguration == nil {
			t.Fatalf("expected NetworkConfiguration, got %+v", nc)
		}
		if got := nc.AwsvpcConfiguration.AssignPublicIp; got != schtypes.AssignPublicIpEnabled {
			t.Errorf("AssignPublicIp = %v, want ENABLED", got)
		}
	})
}

func TestCanonicalizeSchedule(t *testing.T) {
	t.Run("strips UTC timezone, NONE action, default retryPolicy", func(t *testing.T) {
		s := &Schedule{
			ScheduleExpressionTimezone: "UTC",
			ActionAfterCompletion:      "NONE",
			Target: &ScheduleTarget{
				RetryPolicy: &RetryPolicy{
					MaximumRetryAttempts:     185,
					MaximumEventAgeInSeconds: 86400,
				},
			},
		}
		got := canonicalizeSchedule(s)
		if got.ScheduleExpressionTimezone != "" {
			t.Errorf("UTC timezone not stripped: %q", got.ScheduleExpressionTimezone)
		}
		if got.ActionAfterCompletion != "" {
			t.Errorf("NONE action not stripped: %q", got.ActionAfterCompletion)
		}
		if got.Target.RetryPolicy != nil {
			t.Errorf("default RetryPolicy not stripped: %+v", got.Target.RetryPolicy)
		}
		// Non-destructive: original must be unchanged.
		if s.ScheduleExpressionTimezone != "UTC" {
			t.Errorf("input mutated; ScheduleExpressionTimezone = %q", s.ScheduleExpressionTimezone)
		}
		if s.ActionAfterCompletion != "NONE" {
			t.Errorf("input mutated; ActionAfterCompletion = %q", s.ActionAfterCompletion)
		}
		if s.Target.RetryPolicy == nil {
			t.Error("input mutated; original RetryPolicy was cleared")
		}
	})

	t.Run("keeps non-default values", func(t *testing.T) {
		s := &Schedule{
			ScheduleExpressionTimezone: "Asia/Tokyo",
			ActionAfterCompletion:      "DELETE",
			Target: &ScheduleTarget{
				RetryPolicy: &RetryPolicy{
					MaximumRetryAttempts:     3,
					MaximumEventAgeInSeconds: 600,
				},
			},
		}
		got := canonicalizeSchedule(s)
		if got.ScheduleExpressionTimezone != "Asia/Tokyo" {
			t.Errorf("timezone changed: %q", got.ScheduleExpressionTimezone)
		}
		if got.ActionAfterCompletion != "DELETE" {
			t.Errorf("action changed: %q", got.ActionAfterCompletion)
		}
		if got.Target.RetryPolicy == nil || got.Target.RetryPolicy.MaximumRetryAttempts != 3 {
			t.Errorf("RetryPolicy changed: %+v", got.Target.RetryPolicy)
		}
	})

	t.Run("partial default retryPolicy not stripped", func(t *testing.T) {
		s := &Schedule{
			Target: &ScheduleTarget{
				RetryPolicy: &RetryPolicy{
					MaximumRetryAttempts:     185,
					MaximumEventAgeInSeconds: 3600,
				},
			},
		}
		got := canonicalizeSchedule(s)
		if got.Target.RetryPolicy == nil {
			t.Error("partial default RetryPolicy should not be stripped")
		}
	})

	t.Run("nil and empty inputs safe", func(t *testing.T) {
		if canonicalizeSchedule(nil) != nil {
			t.Error("expected nil for nil input")
		}
		canonicalizeSchedule(&Schedule{})
		canonicalizeSchedule(&Schedule{Target: &ScheduleTarget{}})
	})
}

func TestFromRemoteSchedule_FullFields(t *testing.T) {
	startStr := "2026-06-01T00:00:00Z"
	endStr := "2026-06-02T00:00:00Z"
	startT, _ := parseTime(startStr)
	endT, _ := parseTime(endStr)
	in := &scheduler.GetScheduleOutput{
		Name:                       aws.String("s1"),
		Description:                aws.String("desc"),
		GroupName:                  aws.String("my-group"),
		ScheduleExpression:         aws.String("rate(5 minutes)"),
		ScheduleExpressionTimezone: aws.String("Asia/Tokyo"),
		State:                      schtypes.ScheduleStateEnabled,
		StartDate:                  startT,
		EndDate:                    endT,
		KmsKeyArn:                  aws.String("arn:aws:kms:..."),
		ActionAfterCompletion:      schtypes.ActionAfterCompletionDelete,
		FlexibleTimeWindow: &schtypes.FlexibleTimeWindow{
			Mode:                   schtypes.FlexibleTimeWindowModeFlexible,
			MaximumWindowInMinutes: aws.Int32(15),
		},
		Target: &schtypes.Target{
			Arn:     aws.String("arn:aws:lambda:us-east-1:1:function:f"),
			RoleArn: aws.String("arn:aws:iam::1:role/r"),
		},
	}
	got := fromRemoteSchedule(in)

	want := &Schedule{
		Name:                       "s1",
		Description:                "desc",
		GroupName:                  "my-group",
		ScheduleExpression:         "rate(5 minutes)",
		ScheduleExpressionTimezone: "Asia/Tokyo",
		State:                      "ENABLED",
		StartDate:                  startStr,
		EndDate:                    endStr,
		KmsKeyArn:                  "arn:aws:kms:...",
		ActionAfterCompletion:      "DELETE",
		FlexibleTimeWindow: &FlexibleTimeWindow{
			Mode:                   "FLEXIBLE",
			MaximumWindowInMinutes: 15,
		},
		Target: &ScheduleTarget{
			Arn:     "arn:aws:lambda:us-east-1:1:function:f",
			RoleArn: "arn:aws:iam::1:role/r",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got: %+v\nwant: %+v", got, want)
	}
}

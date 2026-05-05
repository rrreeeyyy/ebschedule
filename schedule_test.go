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
			LaunchType:        "FARGATE",
			PlatformVersion:   "LATEST",
			Subnets:           []string{"subnet-a"},
			SecurityGroups:    []string{"sg-1"},
			AssignPublicIp:    "DISABLED",
		},
		SqsParameters: &SqsParameters{
			MessageGroupId: "g1",
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
		canonicalizeSchedule(s)
		if s.ScheduleExpressionTimezone != "" {
			t.Errorf("UTC timezone not stripped: %q", s.ScheduleExpressionTimezone)
		}
		if s.ActionAfterCompletion != "" {
			t.Errorf("NONE action not stripped: %q", s.ActionAfterCompletion)
		}
		if s.Target.RetryPolicy != nil {
			t.Errorf("default RetryPolicy not stripped: %+v", s.Target.RetryPolicy)
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
		before := *s
		beforeRP := *s.Target.RetryPolicy
		canonicalizeSchedule(s)
		if s.ScheduleExpressionTimezone != before.ScheduleExpressionTimezone {
			t.Errorf("timezone changed: %q", s.ScheduleExpressionTimezone)
		}
		if s.ActionAfterCompletion != before.ActionAfterCompletion {
			t.Errorf("action changed: %q", s.ActionAfterCompletion)
		}
		if s.Target.RetryPolicy == nil || *s.Target.RetryPolicy != beforeRP {
			t.Errorf("RetryPolicy changed: %+v", s.Target.RetryPolicy)
		}
	})

	t.Run("partial default retryPolicy not stripped", func(t *testing.T) {
		// Only one of the two fields matches the default; should keep both.
		s := &Schedule{
			Target: &ScheduleTarget{
				RetryPolicy: &RetryPolicy{
					MaximumRetryAttempts:     185,
					MaximumEventAgeInSeconds: 3600,
				},
			},
		}
		canonicalizeSchedule(s)
		if s.Target.RetryPolicy == nil {
			t.Error("partial default RetryPolicy should not be stripped")
		}
	})

	t.Run("nil schedule and nil target safe", func(t *testing.T) {
		canonicalizeSchedule(nil)
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

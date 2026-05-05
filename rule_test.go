package main

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

func TestFromRemoteRule(t *testing.T) {
	r := ebtypes.Rule{
		Name:               aws.String("hello"),
		Description:        aws.String("desc"),
		ScheduleExpression: aws.String("cron(0 9 * * ? *)"),
		State:              ebtypes.RuleStateEnabled,
		RoleArn:            aws.String("arn:aws:iam::1:role/x"),
	}
	got := fromRemoteRule(r)
	want := &Rule{
		Name:               "hello",
		Description:        "desc",
		ScheduleExpression: "cron(0 9 * * ? *)",
		State:              "ENABLED",
		RoleArn:            "arn:aws:iam::1:role/x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got: %+v\nwant: %+v", got, want)
	}
}

// fullTarget exercises every optional field so the round-trip catches
// regressions in a single fixture.
func fullTarget() *Target {
	return &Target{
		ID:        "t1",
		Arn:       "arn:aws:ecs:us-east-1:1:cluster/c",
		RoleArn:   "arn:aws:iam::1:role/r",
		Input:     `{"k":"v"}`,
		InputPath: "$.detail",
		InputTransformer: &InputTransformer{
			InputPathsMap: map[string]string{"a": "$.x"},
			InputTemplate: `{"a": <a>}`,
		},
		RetryPolicy: &RetryPolicy{
			MaximumRetryAttempts:     3,
			MaximumEventAgeInSeconds: 3600,
		},
		DeadLetterConfig: &DeadLetterConfig{Arn: "arn:aws:sqs:us-east-1:1:q"},
		EcsParameters: &RuleEcsParameters{
			TaskDefinitionArn: "arn:aws:ecs:us-east-1:1:task-definition/x:1",
			TaskCount:         2,
			LaunchType:        "FARGATE",
			PlatformVersion:   "LATEST",
			Subnets:           []string{"subnet-aaa", "subnet-bbb"},
			SecurityGroups:    []string{"sg-x"},
			AssignPublicIp:    "DISABLED",
			Group:             "g",
			PropagateTags:     "TASK_DEFINITION",
		},
		SqsParameters:     &SqsParameters{MessageGroupId: "fifo-1"},
		KinesisParameters: &RuleKinesisParameters{PartitionKeyPath: "$.id"},
		BatchParameters: &BatchParameters{
			JobDefinition: "arn:aws:batch:us-east-1:1:job-definition/x:1",
			JobName:       "nightly",
			ArraySize:     5,
			RetryAttempts: 2,
		},
		RedshiftDataParameters: &RedshiftDataParameters{
			Database:         "warehouse",
			DbUser:           "etl",
			SecretManagerArn: "arn:aws:secretsmanager:us-east-1:1:secret/x",
			Sql:              "SELECT 1",
			StatementName:    "stmt1",
			WithEvent:        true,
		},
		SageMakerPipelineParameters: &SageMakerPipelineParameters{
			PipelineParameterList: []SageMakerPipelineParameter{
				{Name: "p1", Value: "v1"},
				{Name: "p2", Value: "v2"},
			},
		},
		HttpParameters: &HttpParameters{
			HeaderParameters:      map[string]string{"X-Auth": "secret"},
			PathParameterValues:   []string{"id-1"},
			QueryStringParameters: map[string]string{"q": "v"},
		},
	}
}

func TestRuleTarget_RoundTrip(t *testing.T) {
	src := fullTarget()
	got := fromRemoteTarget(toAWSTarget(src))
	if !reflect.DeepEqual(got, src) {
		t.Errorf("round-trip mismatch\n got: %+v\nwant: %+v", got, src)
		// Drill into ECS params for easier debugging.
		if !reflect.DeepEqual(got.EcsParameters, src.EcsParameters) {
			t.Errorf("ecs got: %+v\necs want: %+v", got.EcsParameters, src.EcsParameters)
		}
	}
}

func TestRuleTarget_MinimalRoundTrip(t *testing.T) {
	src := &Target{ID: "t1", Arn: "arn:aws:lambda:us-east-1:1:function:f"}
	got := fromRemoteTarget(toAWSTarget(src))
	if !reflect.DeepEqual(got, src) {
		t.Errorf("minimal round-trip\n got: %+v\nwant: %+v", got, src)
	}
}

// toAWSTarget should leave optional fields nil rather than producing
// aws.String("") -- that distinction matters because PutTargets treats empty
// vs nil differently for some fields (e.g. Input).
func TestToAWSTarget_OmitsEmptyOptionalFields(t *testing.T) {
	at := toAWSTarget(&Target{ID: "t1", Arn: "arn:..."})
	if at.RoleArn != nil {
		t.Errorf("RoleArn should be nil for empty source, got %q", *at.RoleArn)
	}
	if at.Input != nil {
		t.Errorf("Input should be nil for empty source")
	}
	if at.InputPath != nil {
		t.Errorf("InputPath should be nil for empty source")
	}
	if at.InputTransformer != nil {
		t.Errorf("InputTransformer should be nil for empty source")
	}
	if at.RetryPolicy != nil {
		t.Errorf("RetryPolicy should be nil for empty source")
	}
	if at.DeadLetterConfig != nil {
		t.Errorf("DeadLetterConfig should be nil for empty source")
	}
	if at.EcsParameters != nil {
		t.Errorf("EcsParameters should be nil for empty source")
	}
}

// EcsParameters has subtle conditional logic; check NetworkConfiguration is
// only attached when Subnets is non-empty.
func TestToAWSTarget_EcsNetworkConfigOnlyWhenSubnets(t *testing.T) {
	t.Run("no subnets -> no network config", func(t *testing.T) {
		at := toAWSTarget(&Target{
			ID: "t", Arn: "arn:...",
			EcsParameters: &RuleEcsParameters{
				TaskDefinitionArn: "arn:...:task-definition/x",
				LaunchType:        "FARGATE",
			},
		})
		if at.EcsParameters.NetworkConfiguration != nil {
			t.Errorf("expected NetworkConfiguration nil when no subnets")
		}
	})
	t.Run("with subnets -> network config attached", func(t *testing.T) {
		at := toAWSTarget(&Target{
			ID: "t", Arn: "arn:...",
			EcsParameters: &RuleEcsParameters{
				TaskDefinitionArn: "arn:...:task-definition/x",
				Subnets:           []string{"s-1"},
				SecurityGroups:    []string{"sg-1"},
				AssignPublicIp:    "ENABLED",
			},
		})
		nc := at.EcsParameters.NetworkConfiguration
		if nc == nil || nc.AwsvpcConfiguration == nil {
			t.Fatalf("expected NetworkConfiguration, got %+v", nc)
		}
		if got := nc.AwsvpcConfiguration.AssignPublicIp; got != ebtypes.AssignPublicIpEnabled {
			t.Errorf("AssignPublicIp = %v, want ENABLED", got)
		}
	})
}

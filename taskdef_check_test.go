package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

type fakeECSDescribe struct {
	calls   []string
	missing map[string]bool
	err     error
}

func (f *fakeECSDescribe) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	arn := *in.TaskDefinition
	f.calls = append(f.calls, arn)
	if f.err != nil {
		return nil, f.err
	}
	if f.missing[arn] {
		return nil, errors.New("ClientException: Unable to describe task definition")
	}
	return &ecs.DescribeTaskDefinitionOutput{}, nil
}

func TestVerifyTaskDefinitionsHappyPath(t *testing.T) {
	cli := &fakeECSDescribe{}
	cfgs := []*Config{{
		Rules: []*Rule{{
			Name: "etl",
			Targets: []*Target{{
				EcsParameters: &RuleEcsParameters{TaskDefinitionArn: "arn:aws:ecs:ap-northeast-1:1:task-definition/etl:5"},
			}},
		}},
	}}
	if err := verifyTaskDefinitions(context.Background(), cli, cfgs); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(cli.calls) != 1 {
		t.Errorf("expected 1 Describe call, got %d", len(cli.calls))
	}
}

func TestVerifyTaskDefinitionsMissingARNErrors(t *testing.T) {
	cli := &fakeECSDescribe{missing: map[string]bool{
		"arn:aws:ecs:ap-northeast-1:1:task-definition/missing:1": true,
	}}
	cfgs := []*Config{{
		Rules: []*Rule{{
			Name: "etl",
			Targets: []*Target{{
				EcsParameters: &RuleEcsParameters{TaskDefinitionArn: "arn:aws:ecs:ap-northeast-1:1:task-definition/missing:1"},
			}},
		}},
	}}
	err := verifyTaskDefinitions(context.Background(), cli, cfgs)
	if err == nil {
		t.Fatal("expected error for missing task def")
	}
	if !strings.Contains(err.Error(), "rule etl") || !strings.Contains(err.Error(), "missing:1") {
		t.Errorf("error should name rule + arn, got %v", err)
	}
}

func TestVerifyTaskDefinitionsCachesByARN(t *testing.T) {
	cli := &fakeECSDescribe{}
	// Two rules pointing at the same task definition. Should describe once.
	td := "arn:aws:ecs:ap-northeast-1:1:task-definition/shared:3"
	cfgs := []*Config{{
		Rules: []*Rule{
			{Name: "a", Targets: []*Target{{EcsParameters: &RuleEcsParameters{TaskDefinitionArn: td}}}},
			{Name: "b", Targets: []*Target{{EcsParameters: &RuleEcsParameters{TaskDefinitionArn: td}}}},
		},
	}}
	if err := verifyTaskDefinitions(context.Background(), cli, cfgs); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(cli.calls) != 1 {
		t.Errorf("shared task def should describe once, got %d", len(cli.calls))
	}
}

func TestVerifyTaskDefinitionsCachedFailureStillErrors(t *testing.T) {
	cli := &fakeECSDescribe{missing: map[string]bool{
		"arn:aws:ecs:ap-northeast-1:1:task-definition/bad:1": true,
	}}
	td := "arn:aws:ecs:ap-northeast-1:1:task-definition/bad:1"
	cfgs := []*Config{{
		Rules: []*Rule{
			{Name: "a", Targets: []*Target{{EcsParameters: &RuleEcsParameters{TaskDefinitionArn: td}}}},
			{Name: "b", Targets: []*Target{{EcsParameters: &RuleEcsParameters{TaskDefinitionArn: td}}}},
		},
	}}
	err := verifyTaskDefinitions(context.Background(), cli, cfgs)
	if err == nil {
		t.Fatal("expected error")
	}
	// Either rule's name is acceptable since the cache might catch it on the
	// second visit, but the message must point at the offending taskDef.
	if !strings.Contains(err.Error(), "bad:1") {
		t.Errorf("error should mention arn, got %v", err)
	}
	// Only one Describe — second call hits the cache.
	if len(cli.calls) != 1 {
		t.Errorf("expected 1 Describe call (cached failure), got %d", len(cli.calls))
	}
}

func TestVerifyTaskDefinitionsSkipsNonECSAndPlaceholders(t *testing.T) {
	cli := &fakeECSDescribe{}
	cfgs := []*Config{{
		Rules: []*Rule{
			// Lambda target — no ecsParameters at all.
			{Name: "lam", Targets: []*Target{{Arn: "arn:aws:lambda:ap-northeast-1:1:function:fn"}}},
			// Placeholder produced by validateFuncs (must_env fallback). Online
			// commands shouldn't see this, but defense-in-depth: skip it.
			{Name: "placeholder", Targets: []*Target{{
				EcsParameters: &RuleEcsParameters{TaskDefinitionArn: "<env:AWS_ACCOUNT_ID>"},
			}}},
		},
	}}
	if err := verifyTaskDefinitions(context.Background(), cli, cfgs); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(cli.calls) != 0 {
		t.Errorf("expected zero Describe calls, got %d (calls=%v)", len(cli.calls), cli.calls)
	}
}

func TestHasECSTarget(t *testing.T) {
	if hasECSTarget(&Config{}) {
		t.Error("empty config should not report ECS")
	}
	cfg := &Config{Rules: []*Rule{{Targets: []*Target{{Arn: "arn:aws:lambda:..."}}}}}
	if hasECSTarget(cfg) {
		t.Error("lambda-only config should not report ECS")
	}
	cfg.Rules[0].Targets[0].EcsParameters = &RuleEcsParameters{}
	if !hasECSTarget(cfg) {
		t.Error("config with ecsParameters target should report ECS")
	}
}

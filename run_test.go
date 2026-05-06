package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
)

type fakeECSRun struct {
	calls []*ecs.RunTaskInput
	resp  *ecs.RunTaskOutput
	err   error
}

func (f *fakeECSRun) RunTask(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &ecs.RunTaskOutput{
		Tasks: []ecstypes.Task{{TaskArn: aws.String("arn:aws:ecs:ap-northeast-1:111111111111:task/cluster/abc")}},
	}, nil
}

type fakeLambdaInvoke struct {
	calls []*lambda.InvokeInput
	resp  *lambda.InvokeOutput
	err   error
}

func (f *fakeLambdaInvoke) Invoke(_ context.Context, in *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &lambda.InvokeOutput{StatusCode: 200, Payload: []byte(`{"ok":true}`)}, nil
}

type fakeSFNStart struct {
	calls []*sfn.StartExecutionInput
	resp  *sfn.StartExecutionOutput
	err   error
}

func (f *fakeSFNStart) StartExecution(_ context.Context, in *sfn.StartExecutionInput, _ ...func(*sfn.Options)) (*sfn.StartExecutionOutput, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &sfn.StartExecutionOutput{
		ExecutionArn: aws.String("arn:aws:states:ap-northeast-1:111111111111:execution:my-sm:exec-1"),
	}, nil
}

func newFakeRunClients() (*runClients, *fakeECSRun, *fakeLambdaInvoke, *fakeSFNStart) {
	e, l, s := &fakeECSRun{}, &fakeLambdaInvoke{}, &fakeSFNStart{}
	return &runClients{ECS: e, Lambda: l, SFN: s}, e, l, s
}

func TestClassifyTarget(t *testing.T) {
	cases := []struct {
		name string
		tgt  *Target
		want targetKind
		err  bool
	}{
		{
			name: "ecs",
			tgt: &Target{
				Arn:           "arn:aws:ecs:ap-northeast-1:111111111111:cluster/example",
				EcsParameters: &RuleEcsParameters{TaskDefinitionArn: "arn:aws:ecs:ap-northeast-1:111111111111:task-definition/foo:1"},
			},
			want: targetKindECS,
		},
		{
			name: "ecs without ecsParameters falls through",
			tgt:  &Target{Arn: "arn:aws:ecs:ap-northeast-1:111111111111:cluster/example"},
			err:  true,
		},
		{
			name: "lambda",
			tgt:  &Target{Arn: "arn:aws:lambda:ap-northeast-1:111111111111:function:my-fn"},
			want: targetKindLambda,
		},
		{
			name: "sfn",
			tgt:  &Target{Arn: "arn:aws:states:ap-northeast-1:111111111111:stateMachine:my-sm"},
			want: targetKindSFN,
		},
		{
			name: "sqs unsupported",
			tgt:  &Target{Arn: "arn:aws:sqs:ap-northeast-1:111111111111:my-queue"},
			err:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyTarget(tc.tgt)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got kind=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("kind: want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestRunRuleECS(t *testing.T) {
	cli, ecsCli, _, _ := newFakeRunClients()
	rule := &Rule{
		Name: "etl",
		Targets: []*Target{{
			ID:  "ecs",
			Arn: "arn:aws:ecs:ap-northeast-1:111111111111:cluster/example",
			EcsParameters: &RuleEcsParameters{
				TaskDefinitionArn: "arn:aws:ecs:ap-northeast-1:111111111111:task-definition/etl:5",
				LaunchType:        "FARGATE",
				TaskCount:         2,
				Subnets:           []string{"subnet-aaa"},
				SecurityGroups:    []string{"sg-1"},
				AssignPublicIp:    "DISABLED",
				Tags:              []KeyValuePair{{Name: "Owner", Value: "platform"}},
			},
		}},
	}
	var out bytes.Buffer
	if err := runRule(context.Background(), &out, cli, rule, false); err != nil {
		t.Fatalf("runRule: %v", err)
	}
	if len(ecsCli.calls) != 1 {
		t.Fatalf("expected 1 RunTask call, got %d", len(ecsCli.calls))
	}
	in := ecsCli.calls[0]
	if aws.ToString(in.Cluster) != "arn:aws:ecs:ap-northeast-1:111111111111:cluster/example" {
		t.Errorf("cluster: %s", aws.ToString(in.Cluster))
	}
	if aws.ToString(in.TaskDefinition) != "arn:aws:ecs:ap-northeast-1:111111111111:task-definition/etl:5" {
		t.Errorf("taskdef: %s", aws.ToString(in.TaskDefinition))
	}
	if aws.ToInt32(in.Count) != 2 {
		t.Errorf("count: %d", aws.ToInt32(in.Count))
	}
	if in.LaunchType != ecstypes.LaunchTypeFargate {
		t.Errorf("launchType: %s", in.LaunchType)
	}
	if in.NetworkConfiguration == nil || in.NetworkConfiguration.AwsvpcConfiguration == nil {
		t.Fatal("networkConfiguration missing")
	}
	if got := in.NetworkConfiguration.AwsvpcConfiguration.AssignPublicIp; got != ecstypes.AssignPublicIpDisabled {
		t.Errorf("assignPublicIp: %s", got)
	}
	if len(in.Tags) != 1 || aws.ToString(in.Tags[0].Key) != "Owner" {
		t.Errorf("tags: %#v", in.Tags)
	}
	if !strings.Contains(out.String(), "ecs:RunTask started") {
		t.Errorf("expected success log, got %q", out.String())
	}
}

func TestRunRuleECSDefaultsCountToOne(t *testing.T) {
	cli, ecsCli, _, _ := newFakeRunClients()
	rule := &Rule{
		Name: "etl",
		Targets: []*Target{{
			ID:  "ecs",
			Arn: "arn:aws:ecs:ap-northeast-1:111111111111:cluster/example",
			EcsParameters: &RuleEcsParameters{
				TaskDefinitionArn: "arn:aws:ecs:ap-northeast-1:111111111111:task-definition/etl:5",
				// TaskCount intentionally 0 — apply strips the AWS default
				// of 1 from canonical config, and run must not pass 0.
			},
		}},
	}
	if err := runRule(context.Background(), &bytes.Buffer{}, cli, rule, false); err != nil {
		t.Fatalf("runRule: %v", err)
	}
	if got := aws.ToInt32(ecsCli.calls[0].Count); got != 1 {
		t.Errorf("count: want 1, got %d", got)
	}
}

func TestRunRuleECSReportsFailures(t *testing.T) {
	cli, ecsCli, _, _ := newFakeRunClients()
	ecsCli.resp = &ecs.RunTaskOutput{
		Failures: []ecstypes.Failure{{
			Arn:    aws.String("arn:aws:ecs:ap-northeast-1:111111111111:container-instance/x"),
			Reason: aws.String("RESOURCE:CPU"),
			Detail: aws.String("not enough cpu"),
		}},
	}
	rule := &Rule{
		Name: "etl",
		Targets: []*Target{{
			Arn:           "arn:aws:ecs:ap-northeast-1:111111111111:cluster/example",
			EcsParameters: &RuleEcsParameters{TaskDefinitionArn: "arn:aws:ecs:ap-northeast-1:111111111111:task-definition/x:1"},
		}},
	}
	err := runRule(context.Background(), &bytes.Buffer{}, cli, rule, false)
	if err == nil {
		t.Fatal("expected failure error")
	}
	if !strings.Contains(err.Error(), "RESOURCE:CPU") {
		t.Errorf("error should mention failure reason, got %v", err)
	}
}

func TestRunRuleLambda(t *testing.T) {
	cli, _, lam, _ := newFakeRunClients()
	rule := &Rule{
		Name: "ping",
		Targets: []*Target{{
			ID:    "lambda",
			Arn:   "arn:aws:lambda:ap-northeast-1:111111111111:function:my-fn",
			Input: jsonField(`{"hello":"world"}`),
		}},
	}
	var out bytes.Buffer
	if err := runRule(context.Background(), &out, cli, rule, false); err != nil {
		t.Fatalf("runRule: %v", err)
	}
	if len(lam.calls) != 1 {
		t.Fatalf("expected 1 Invoke call, got %d", len(lam.calls))
	}
	if string(lam.calls[0].Payload) != `{"hello":"world"}` {
		t.Errorf("payload: %s", lam.calls[0].Payload)
	}
	if !strings.Contains(out.String(), "lambda:Invoke status=200") {
		t.Errorf("expected status log, got %q", out.String())
	}
}

func TestRunRuleLambdaSurfacesFunctionError(t *testing.T) {
	cli, _, lam, _ := newFakeRunClients()
	lam.resp = &lambda.InvokeOutput{
		StatusCode:    200,
		FunctionError: aws.String("Unhandled"),
		Payload:       []byte(`{"errorMessage":"boom"}`),
	}
	rule := &Rule{
		Name: "ping",
		Targets: []*Target{{
			Arn: "arn:aws:lambda:ap-northeast-1:111111111111:function:my-fn",
		}},
	}
	err := runRule(context.Background(), &bytes.Buffer{}, cli, rule, false)
	if err == nil || !strings.Contains(err.Error(), "Unhandled") {
		t.Errorf("expected Unhandled error, got %v", err)
	}
}

func TestRunRuleSFN(t *testing.T) {
	cli, _, _, sm := newFakeRunClients()
	rule := &Rule{
		Name: "wf",
		Targets: []*Target{{
			ID:    "sfn",
			Arn:   "arn:aws:states:ap-northeast-1:111111111111:stateMachine:my-sm",
			Input: jsonField(`{"k":1}`),
		}},
	}
	var out bytes.Buffer
	if err := runRule(context.Background(), &out, cli, rule, false); err != nil {
		t.Fatalf("runRule: %v", err)
	}
	if len(sm.calls) != 1 {
		t.Fatalf("expected 1 StartExecution call, got %d", len(sm.calls))
	}
	in := sm.calls[0]
	if aws.ToString(in.Input) != `{"k":1}` {
		t.Errorf("input: %s", aws.ToString(in.Input))
	}
	if !strings.HasPrefix(aws.ToString(in.Name), "ebschedule-run-wf-") {
		t.Errorf("name: %s", aws.ToString(in.Name))
	}
	if !strings.Contains(out.String(), "sfn:StartExecution executionArn=") {
		t.Errorf("expected executionArn log, got %q", out.String())
	}
}

func TestRunRuleSFNDefaultsInputToEmptyObject(t *testing.T) {
	cli, _, _, sm := newFakeRunClients()
	rule := &Rule{
		Name: "wf",
		Targets: []*Target{{
			Arn: "arn:aws:states:ap-northeast-1:111111111111:stateMachine:my-sm",
			// no Input set
		}},
	}
	if err := runRule(context.Background(), &bytes.Buffer{}, cli, rule, false); err != nil {
		t.Fatalf("runRule: %v", err)
	}
	if got := aws.ToString(sm.calls[0].Input); got != "{}" {
		t.Errorf("input: want {}, got %s", got)
	}
}

func TestRunRuleDryRunSkipsAWS(t *testing.T) {
	cli, ecsCli, lam, sm := newFakeRunClients()
	rule := &Rule{
		Name: "multi",
		Targets: []*Target{
			{
				Arn:           "arn:aws:ecs:ap-northeast-1:111111111111:cluster/example",
				EcsParameters: &RuleEcsParameters{TaskDefinitionArn: "arn:aws:ecs:ap-northeast-1:111111111111:task-definition/x:1"},
			},
			{Arn: "arn:aws:lambda:ap-northeast-1:111111111111:function:fn"},
			{Arn: "arn:aws:states:ap-northeast-1:111111111111:stateMachine:sm"},
		},
	}
	var out bytes.Buffer
	if err := runRule(context.Background(), &out, cli, rule, true); err != nil {
		t.Fatalf("runRule: %v", err)
	}
	if len(ecsCli.calls)+len(lam.calls)+len(sm.calls) != 0 {
		t.Fatalf("dry-run must not call AWS, got ecs=%d lambda=%d sfn=%d",
			len(ecsCli.calls), len(lam.calls), len(sm.calls))
	}
	if !strings.Contains(out.String(), "[dry-run] ecs:RunTask") ||
		!strings.Contains(out.String(), "[dry-run] lambda:Invoke") ||
		!strings.Contains(out.String(), "[dry-run] sfn:StartExecution") {
		t.Errorf("expected dry-run log per target, got %q", out.String())
	}
}

func TestRunRuleUnsupportedTarget(t *testing.T) {
	cli, _, _, _ := newFakeRunClients()
	rule := &Rule{
		Name: "queue-fan-out",
		Targets: []*Target{{
			ID:  "sqs",
			Arn: "arn:aws:sqs:ap-northeast-1:111111111111:my-queue",
		}},
	}
	err := runRule(context.Background(), &bytes.Buffer{}, cli, rule, false)
	if err == nil || !strings.Contains(err.Error(), "supported invocation type") {
		t.Errorf("expected unsupported error, got %v", err)
	}
}

func TestRunRuleMultiTargetStopsOnFirstError(t *testing.T) {
	cli, _, lam, sm := newFakeRunClients()
	lam.err = errors.New("invoke boom")
	rule := &Rule{
		Name: "multi",
		Targets: []*Target{
			{ID: "lam", Arn: "arn:aws:lambda:ap-northeast-1:111111111111:function:fn"},
			{ID: "sm", Arn: "arn:aws:states:ap-northeast-1:111111111111:stateMachine:sm"},
		},
	}
	err := runRule(context.Background(), &bytes.Buffer{}, cli, rule, false)
	if err == nil || !strings.Contains(err.Error(), "invoke boom") {
		t.Fatalf("expected lambda error, got %v", err)
	}
	if len(sm.calls) != 0 {
		t.Errorf("subsequent target should not be invoked after first error, got %d sfn calls", len(sm.calls))
	}
}

func TestRunRuleEmptyTargetsErrors(t *testing.T) {
	cli, _, _, _ := newFakeRunClients()
	err := runRule(context.Background(), &bytes.Buffer{}, cli, &Rule{Name: "empty"}, false)
	if err == nil || !strings.Contains(err.Error(), "no targets") {
		t.Errorf("expected empty-targets error, got %v", err)
	}
}

func TestFindRule(t *testing.T) {
	cfgs := []*Config{
		{
			Rules: []*Rule{{Name: "etl"}, {Name: "rollup"}},
		},
		{
			Rules: []*Rule{{Name: "ingest"}},
		},
	}
	if _, r, err := findRule(cfgs, "rollup"); err != nil || r.Name != "rollup" {
		t.Errorf("findRule rollup: rule=%v err=%v", r, err)
	}
	if _, r, err := findRule(cfgs, "ingest"); err != nil || r.Name != "ingest" {
		t.Errorf("findRule ingest: rule=%v err=%v", r, err)
	}
	_, _, err := findRule(cfgs, "missing")
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "etl") || !strings.Contains(err.Error(), "rollup") || !strings.Contains(err.Error(), "ingest") {
		t.Errorf("error should list available rules, got %v", err)
	}
}

func TestRunRuleECSAppliesContainerOverridesFromInput(t *testing.T) {
	cli, ecsCli, _, _ := newFakeRunClients()
	// The exact JSON shape import-ecschedule emits.
	input := `{"containerOverrides":[{"name":"app","command":["migrate"],"environment":[{"name":"DEBUG","value":"1"}]}],"taskOverride":{"cpu":"512","memory":"1024"}}`
	rule := &Rule{
		Name: "etl",
		Targets: []*Target{{
			ID:    "ecs",
			Arn:   "arn:aws:ecs:ap-northeast-1:111111111111:cluster/example",
			Input: jsonField(input),
			EcsParameters: &RuleEcsParameters{
				TaskDefinitionArn: "arn:aws:ecs:ap-northeast-1:111111111111:task-definition/etl:5",
			},
		}},
	}
	if err := runRule(context.Background(), &bytes.Buffer{}, cli, rule, false); err != nil {
		t.Fatalf("runRule: %v", err)
	}
	in := ecsCli.calls[0]
	if in.Overrides == nil {
		t.Fatal("expected RunTask.Overrides to be set from target.Input")
	}
	if aws.ToString(in.Overrides.Cpu) != "512" || aws.ToString(in.Overrides.Memory) != "1024" {
		t.Errorf("taskOverride mismatch: cpu=%s memory=%s",
			aws.ToString(in.Overrides.Cpu), aws.ToString(in.Overrides.Memory))
	}
	if len(in.Overrides.ContainerOverrides) != 1 {
		t.Fatalf("expected 1 ContainerOverride, got %d", len(in.Overrides.ContainerOverrides))
	}
	co := in.Overrides.ContainerOverrides[0]
	if aws.ToString(co.Name) != "app" {
		t.Errorf("override name: %s", aws.ToString(co.Name))
	}
	if len(co.Command) != 1 || co.Command[0] != "migrate" {
		t.Errorf("command: %v", co.Command)
	}
	if len(co.Environment) != 1 || aws.ToString(co.Environment[0].Name) != "DEBUG" {
		t.Errorf("env: %+v", co.Environment)
	}
}

func TestRunRuleECSEmptyInputLeavesOverridesUnset(t *testing.T) {
	cli, ecsCli, _, _ := newFakeRunClients()
	rule := &Rule{
		Name: "etl",
		Targets: []*Target{{
			Arn: "arn:aws:ecs:ap-northeast-1:111111111111:cluster/example",
			EcsParameters: &RuleEcsParameters{
				TaskDefinitionArn: "arn:aws:ecs:ap-northeast-1:111111111111:task-definition/etl:5",
			},
		}},
	}
	if err := runRule(context.Background(), &bytes.Buffer{}, cli, rule, false); err != nil {
		t.Fatalf("runRule: %v", err)
	}
	if ecsCli.calls[0].Overrides != nil {
		t.Errorf("expected Overrides to stay nil with empty input, got %+v", ecsCli.calls[0].Overrides)
	}
}

func TestParseECSOverrides(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantNil     bool
		wantCpu     string
		wantCount   int
		wantContain string
	}{
		{name: "empty returns nil", in: "", wantNil: true},
		{name: "container only", in: `{"containerOverrides":[{"name":"a"}]}`, wantCount: 1},
		{name: "task only", in: `{"taskOverride":{"cpu":"256"}}`, wantCpu: "256"},
		{name: "both", in: `{"containerOverrides":[{"name":"a"},{"name":"b"}],"taskOverride":{"cpu":"1024","memory":"2048"}}`, wantCount: 2, wantCpu: "1024"},
		{name: "extra keys tolerated", in: `{"foo":"bar","containerOverrides":[{"name":"x"}]}`, wantCount: 1},
		{name: "object without overrides returns nil", in: `{"foo":"bar"}`, wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseECSOverrides(jsonField(tc.in))
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil")
			}
			if len(got.ContainerOverrides) != tc.wantCount {
				t.Errorf("count: want %d, got %d", tc.wantCount, len(got.ContainerOverrides))
			}
			if tc.wantCpu != "" && aws.ToString(got.Cpu) != tc.wantCpu {
				t.Errorf("cpu: want %s, got %s", tc.wantCpu, aws.ToString(got.Cpu))
			}
		})
	}
}

func TestParseECSOverridesInvalidJSONErrors(t *testing.T) {
	if _, err := parseECSOverrides(jsonField("not-json")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestPayloadFromInput(t *testing.T) {
	cases := []struct {
		in   jsonField
		want string
	}{
		{"", "{}"},
		{`{"a":1}`, `{"a":1}`},
		{"   ", "{}"},
		{"not-json", "{}"}, // safety net for hand-built configs
	}
	for _, tc := range cases {
		got := string(payloadFromInput(tc.in))
		if got != tc.want {
			t.Errorf("payloadFromInput(%q): want %q, got %q", string(tc.in), tc.want, got)
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
)

// ecsRunAPI / lambdaInvokeAPI / sfnStartAPI are the per-service subsets used
// by the run subcommand. Defining them here lets tests inject fakes; the
// real *ecs.Client / *lambda.Client / *sfn.Client implement them implicitly.
type ecsRunAPI interface {
	RunTask(ctx context.Context, in *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
}

type lambdaInvokeAPI interface {
	Invoke(ctx context.Context, in *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

type sfnStartAPI interface {
	StartExecution(ctx context.Context, in *sfn.StartExecutionInput, optFns ...func(*sfn.Options)) (*sfn.StartExecutionOutput, error)
}

// runClients bundles the per-service AWS clients run dispatches to. Tests
// pass fakes; production wiring constructs real ones in newRunClients.
type runClients struct {
	ECS    ecsRunAPI
	Lambda lambdaInvokeAPI
	SFN    sfnStartAPI
}

// targetKind enumerates the target shapes the run subcommand can dispatch.
type targetKind int

const (
	targetKindUnsupported targetKind = iota
	targetKindECS
	targetKindLambda
	targetKindSFN
)

// classifyTarget inspects t.Arn (with t.EcsParameters as a tiebreaker for
// ECS) to decide which AWS API to call. Returns targetKindUnsupported with
// a descriptive error for everything ebschedule run does not handle yet
// (SQS, SNS, Kinesis, Batch, Redshift Data, API Destination, etc.).
func classifyTarget(t *Target) (targetKind, error) {
	arn := t.Arn
	switch {
	case strings.HasPrefix(arn, "arn:aws:ecs:") && t.EcsParameters != nil:
		return targetKindECS, nil
	case strings.HasPrefix(arn, "arn:aws:lambda:") && strings.Contains(arn, ":function:"):
		return targetKindLambda, nil
	case strings.HasPrefix(arn, "arn:aws:states:") && strings.Contains(arn, ":stateMachine:"):
		return targetKindSFN, nil
	default:
		return targetKindUnsupported, fmt.Errorf("run: target %q (arn=%q) is not a supported invocation type (ECS RunTask / Lambda Invoke / Step Functions StartExecution)", t.ID, arn)
	}
}

// findRule returns the first rule whose Name matches across all configs,
// or an error listing the names available so a typo is easy to spot.
func findRule(cfgs []*Config, name string) (*Config, *Rule, error) {
	var names []string
	for _, c := range cfgs {
		for _, r := range c.Rules {
			if r.Name == name {
				return c, r, nil
			}
			names = append(names, r.Name)
		}
	}
	return nil, nil, fmt.Errorf("rule %q not found (available: %s)", name, strings.Join(names, ", "))
}

// runRule dispatches every target on the rule. Stops on the first failure
// so an erroring target doesn't get masked by later success output.
func runRule(ctx context.Context, out io.Writer, cli *runClients, r *Rule, dryRun bool) error {
	if len(r.Targets) == 0 {
		return fmt.Errorf("rule %q has no targets to run", r.Name)
	}
	for _, t := range r.Targets {
		if err := runTarget(ctx, out, cli, r, t, dryRun); err != nil {
			return fmt.Errorf("target %s: %w", t.ID, err)
		}
	}
	return nil
}

func runTarget(ctx context.Context, out io.Writer, cli *runClients, r *Rule, t *Target, dryRun bool) error {
	kind, err := classifyTarget(t)
	if err != nil {
		return err
	}
	switch kind {
	case targetKindECS:
		return runECSTarget(ctx, out, cli.ECS, t, dryRun)
	case targetKindLambda:
		return runLambdaTarget(ctx, out, cli.Lambda, t, dryRun)
	case targetKindSFN:
		return runSFNTarget(ctx, out, cli.SFN, r, t, dryRun)
	default:
		return fmt.Errorf("run: classifier returned unknown kind for target %q", t.ID)
	}
}

// runECSTarget calls ecs:RunTask using ecsParameters straight from the
// rule. target.Input is parsed for the ecschedule-shaped
// `{containerOverrides, taskOverride}` payload (that's how
// import-ecschedule encodes the YAML override block) and translated
// into RunTask Overrides — so an ecschedule user who imports their
// config and runs `ebschedule run` keeps the same override behavior
// they had with `ecschedule run`.
func runECSTarget(ctx context.Context, out io.Writer, cli ecsRunAPI, t *Target, dryRun bool) error {
	cluster := t.Arn
	ep := t.EcsParameters
	overrides, err := parseECSOverrides(t.Input)
	if err != nil {
		return fmt.Errorf("ecs:RunTask overrides: %w", err)
	}
	if dryRun {
		ovr := ""
		if overrides != nil {
			ovr = fmt.Sprintf(" containerOverrides=%d", len(overrides.ContainerOverrides))
		}
		fmt.Fprintf(out, "[dry-run] ecs:RunTask cluster=%s taskDefinition=%s launchType=%s count=%d%s\n",
			cluster, ep.TaskDefinitionArn, ep.LaunchType, ecsRunTaskCount(ep), ovr)
		return nil
	}
	in := &ecs.RunTaskInput{
		Cluster:        aws.String(cluster),
		TaskDefinition: aws.String(ep.TaskDefinitionArn),
		Count:          aws.Int32(ecsRunTaskCount(ep)),
	}
	if ep.LaunchType != "" {
		in.LaunchType = ecstypes.LaunchType(ep.LaunchType)
	}
	if ep.PlatformVersion != "" {
		in.PlatformVersion = aws.String(ep.PlatformVersion)
	}
	if ep.Group != "" {
		in.Group = aws.String(ep.Group)
	}
	if ep.PropagateTags != "" {
		in.PropagateTags = ecstypes.PropagateTags(ep.PropagateTags)
	}
	if ep.EnableExecuteCommand {
		in.EnableExecuteCommand = ep.EnableExecuteCommand
	}
	if ep.EnableECSManagedTags {
		in.EnableECSManagedTags = ep.EnableECSManagedTags
	}
	if ep.ReferenceID != "" {
		in.ReferenceId = aws.String(ep.ReferenceID)
	}
	if len(ep.Subnets) > 0 || len(ep.SecurityGroups) > 0 || ep.AssignPublicIp != "" {
		in.NetworkConfiguration = &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        ep.Subnets,
				SecurityGroups: ep.SecurityGroups,
				AssignPublicIp: ecstypes.AssignPublicIp(ep.AssignPublicIp),
			},
		}
	}
	for _, c := range ep.CapacityProviderStrategy {
		item := ecstypes.CapacityProviderStrategyItem{
			CapacityProvider: aws.String(c.CapacityProvider),
			Base:             c.Base,
			Weight:           c.Weight,
		}
		in.CapacityProviderStrategy = append(in.CapacityProviderStrategy, item)
	}
	for _, p := range ep.PlacementConstraints {
		in.PlacementConstraints = append(in.PlacementConstraints, ecstypes.PlacementConstraint{
			Type:       ecstypes.PlacementConstraintType(p.Type),
			Expression: aws.String(p.Expression),
		})
	}
	for _, p := range ep.PlacementStrategy {
		in.PlacementStrategy = append(in.PlacementStrategy, ecstypes.PlacementStrategy{
			Type:  ecstypes.PlacementStrategyType(p.Type),
			Field: aws.String(p.Field),
		})
	}
	for _, tg := range ep.Tags {
		in.Tags = append(in.Tags, ecstypes.Tag{
			Key:   aws.String(tg.Name),
			Value: aws.String(tg.Value),
		})
	}
	if overrides != nil {
		in.Overrides = overrides
	}
	resp, err := cli.RunTask(ctx, in)
	if err != nil {
		return fmt.Errorf("ecs:RunTask: %w", err)
	}
	if len(resp.Failures) > 0 {
		f := resp.Failures[0]
		return fmt.Errorf("ecs:RunTask failure arn=%s reason=%s detail=%s",
			aws.ToString(f.Arn), aws.ToString(f.Reason), aws.ToString(f.Detail))
	}
	for _, task := range resp.Tasks {
		fmt.Fprintf(out, "ecs:RunTask started taskArn=%s\n", aws.ToString(task.TaskArn))
	}
	return nil
}

// ecsRunTaskCount returns the count to pass to RunTask. EventBridge's
// taskCount defaults to 1 (the AWS default we strip on read), so when the
// stored value is 0 we reapply the default rather than asking AWS to run
// zero tasks.
func ecsRunTaskCount(ep *RuleEcsParameters) int32 {
	if ep.TaskCount <= 0 {
		return 1
	}
	return ep.TaskCount
}

// runLambdaTarget calls lambda:Invoke with target.Input as the payload
// (defaulting to "{}" when empty). The response Payload is written to
// out; functional errors are surfaced both via the error return and the
// FunctionError header so users see "Handled" / "Unhandled" in stdout.
func runLambdaTarget(ctx context.Context, out io.Writer, cli lambdaInvokeAPI, t *Target, dryRun bool) error {
	payload := payloadFromInput(t.Input)
	if dryRun {
		fmt.Fprintf(out, "[dry-run] lambda:Invoke function=%s payloadBytes=%d\n", t.Arn, len(payload))
		return nil
	}
	resp, err := cli.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(t.Arn),
		InvocationType: lambdatypes.InvocationTypeRequestResponse,
		Payload:        payload,
	})
	if err != nil {
		return fmt.Errorf("lambda:Invoke: %w", err)
	}
	fmt.Fprintf(out, "lambda:Invoke status=%d", resp.StatusCode)
	if resp.FunctionError != nil {
		fmt.Fprintf(out, " functionError=%s", aws.ToString(resp.FunctionError))
	}
	fmt.Fprintln(out)
	if len(resp.Payload) > 0 {
		// Best-effort: write the response body verbatim. An error here is
		// almost always stdout being closed, in which case the surrounding
		// run already failed; surfacing it would just confuse the user.
		_, _ = out.Write(resp.Payload)
		fmt.Fprintln(out)
	}
	if resp.FunctionError != nil {
		return fmt.Errorf("lambda function returned %s", aws.ToString(resp.FunctionError))
	}
	return nil
}

// runSFNTarget calls sfn:StartExecution. The execution name embeds the
// rule name + a millisecond timestamp so repeated `run` invocations don't
// collide on Step Functions' name-uniqueness window.
func runSFNTarget(ctx context.Context, out io.Writer, cli sfnStartAPI, r *Rule, t *Target, dryRun bool) error {
	input := string(payloadFromInput(t.Input))
	name := fmt.Sprintf("ebschedule-run-%s-%d", r.Name, time.Now().UnixMilli())
	if dryRun {
		fmt.Fprintf(out, "[dry-run] sfn:StartExecution stateMachine=%s name=%s\n", t.Arn, name)
		return nil
	}
	resp, err := cli.StartExecution(ctx, &sfn.StartExecutionInput{
		StateMachineArn: aws.String(t.Arn),
		Input:           aws.String(input),
		Name:            aws.String(name),
	})
	if err != nil {
		return fmt.Errorf("sfn:StartExecution: %w", err)
	}
	fmt.Fprintf(out, "sfn:StartExecution executionArn=%s\n", aws.ToString(resp.ExecutionArn))
	return nil
}

// ecsOverrideEnvelope mirrors the JSON shape that EventBridge's ECS
// RunTask integration accepts on a target's Input field. ecschedule
// emits this exact shape, and import-ecschedule writes it on conversion.
type ecsOverrideEnvelope struct {
	ContainerOverrides []ecsOverrideContainer `json:"containerOverrides,omitempty"`
	TaskOverride       *ecsOverrideTask       `json:"taskOverride,omitempty"`
}

type ecsOverrideContainer struct {
	Name              string             `json:"name"`
	Command           []string           `json:"command,omitempty"`
	Environment       []ecsOverrideEnvKV `json:"environment,omitempty"`
	Cpu               *int32             `json:"cpu,omitempty"`
	Memory            *int32             `json:"memory,omitempty"`
	MemoryReservation *int32             `json:"memoryReservation,omitempty"`
}

type ecsOverrideEnvKV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ecsOverrideTask struct {
	Cpu    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// parseECSOverrides reads target.Input as an ECS override envelope and
// translates it into a TaskOverride for ecs:RunTask. Returns (nil, nil)
// when the input is empty or carries no override fields, so the caller
// can branch cleanly. Unknown top-level keys are tolerated — the goal
// is round-tripping ecschedule-shaped input, not strict validation.
func parseECSOverrides(input jsonField) (*ecstypes.TaskOverride, error) {
	s := strings.TrimSpace(string(input))
	if s == "" {
		return nil, nil
	}
	var env ecsOverrideEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return nil, fmt.Errorf("input is not valid JSON: %w", err)
	}
	if len(env.ContainerOverrides) == 0 && env.TaskOverride == nil {
		return nil, nil
	}
	to := &ecstypes.TaskOverride{}
	for _, co := range env.ContainerOverrides {
		c := ecstypes.ContainerOverride{
			Name:              aws.String(co.Name),
			Command:           co.Command,
			Cpu:               co.Cpu,
			Memory:            co.Memory,
			MemoryReservation: co.MemoryReservation,
		}
		for _, e := range co.Environment {
			c.Environment = append(c.Environment, ecstypes.KeyValuePair{
				Name:  aws.String(e.Name),
				Value: aws.String(e.Value),
			})
		}
		to.ContainerOverrides = append(to.ContainerOverrides, c)
	}
	if env.TaskOverride != nil {
		if env.TaskOverride.Cpu != "" {
			to.Cpu = aws.String(env.TaskOverride.Cpu)
		}
		if env.TaskOverride.Memory != "" {
			to.Memory = aws.String(env.TaskOverride.Memory)
		}
	}
	return to, nil
}

// payloadFromInput renders a target.Input value as raw JSON bytes. Empty
// input becomes "{}" so callers can pass it straight to AWS without an
// extra nil check; canonicalization in jsonField guarantees valid JSON.
func payloadFromInput(input jsonField) []byte {
	s := strings.TrimSpace(string(input))
	if s == "" {
		return []byte("{}")
	}
	// Sanity-check: the field went through canonicalizeJSON on load, but if
	// a caller hand-built a Target without that path we still want to bail
	// rather than ship garbage to AWS.
	var v json.RawMessage
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return []byte("{}")
	}
	return []byte(s)
}

// newRunClients builds the AWS service clients used by the run subcommand.
// Real AWS wiring; tests skip this and construct runClients directly.
func newRunClients(ctx context.Context, region string) (*runClients, error) {
	awsCfg, err := loadAWS(ctx, region)
	if err != nil {
		return nil, err
	}
	return &runClients{
		ECS:    ecs.NewFromConfig(awsCfg),
		Lambda: lambda.NewFromConfig(awsCfg),
		SFN:    sfn.NewFromConfig(awsCfg),
	}, nil
}

// runRunSubcommand parses `ebschedule run -rule NAME [-dry-run]` and
// dispatches the named rule. We honor both the global -dry-run (set on
// the top-level flag.FlagSet) and a subcommand-local one for ecschedule
// CLI compatibility — `ecschedule run -rule X -dry-run` should keep
// working unchanged.
func runRunSubcommand(ctx context.Context, out io.Writer, confPath string, globalDryRun bool, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // surface our own usage on parse failure
	var (
		ruleName string
		localDry bool
	)
	fs.StringVar(&ruleName, "rule", "", "rule name to run (required)")
	fs.BoolVar(&localDry, "dry-run", false, "print what would be invoked without calling AWS")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if ruleName == "" {
		return errors.New("run: -rule NAME is required")
	}
	dryRun := globalDryRun || localDry

	autoResolveAccountEnv(ctx)
	cfgs, err := loadConfigs(confPath)
	if err != nil {
		return err
	}
	cfg, rule, err := findRule(cfgs, ruleName)
	if err != nil {
		return err
	}
	if !dryRun {
		// run only mutates AWS when not in dry-run, so mirror apply's
		// pre-flight gate (catches expired SSO before we issue the call).
		if err := preflightCheck(ctx, []*Config{cfg}); err != nil {
			return fmt.Errorf("pre-flight: %w", err)
		}
	}
	cli, err := newRunClients(ctx, cfg.Region)
	if err != nil {
		return fmt.Errorf("AWS clients: %w", err)
	}
	suffix := ""
	if dryRun {
		suffix = " (dry-run)"
	}
	fmt.Fprintf(out, "running rule %q%s\n", rule.Name, suffix)
	return runRule(ctx, out, cli, rule, dryRun)
}

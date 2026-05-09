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
	"github.com/aws/aws-sdk-go-v2/service/batch"
	batchtypes "github.com/aws/aws-sdk-go-v2/service/batch/types"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/redshiftdata"
	"github.com/aws/aws-sdk-go-v2/service/sagemaker"
	sagemakertypes "github.com/aws/aws-sdk-go-v2/service/sagemaker/types"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
)

// --- API interfaces -------------------------------------------------------
//
// Per-service subsets used by the run subcommand. Defining them here lets
// tests inject fakes; the real *ecs.Client / *lambda.Client / etc. all
// implement these implicitly.

type ecsRunAPI interface {
	RunTask(ctx context.Context, in *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
}

type lambdaInvokeAPI interface {
	Invoke(ctx context.Context, in *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

type sfnStartAPI interface {
	StartExecution(ctx context.Context, in *sfn.StartExecutionInput, optFns ...func(*sfn.Options)) (*sfn.StartExecutionOutput, error)
}

type batchSubmitAPI interface {
	SubmitJob(ctx context.Context, in *batch.SubmitJobInput, optFns ...func(*batch.Options)) (*batch.SubmitJobOutput, error)
}

type glueStartAPI interface {
	StartJobRun(ctx context.Context, in *glue.StartJobRunInput, optFns ...func(*glue.Options)) (*glue.StartJobRunOutput, error)
}

type codebuildStartAPI interface {
	StartBuild(ctx context.Context, in *codebuild.StartBuildInput, optFns ...func(*codebuild.Options)) (*codebuild.StartBuildOutput, error)
}

type codepipelineStartAPI interface {
	StartPipelineExecution(ctx context.Context, in *codepipeline.StartPipelineExecutionInput, optFns ...func(*codepipeline.Options)) (*codepipeline.StartPipelineExecutionOutput, error)
}

type sagemakerStartAPI interface {
	StartPipelineExecution(ctx context.Context, in *sagemaker.StartPipelineExecutionInput, optFns ...func(*sagemaker.Options)) (*sagemaker.StartPipelineExecutionOutput, error)
}

type redshiftDataAPI interface {
	ExecuteStatement(ctx context.Context, in *redshiftdata.ExecuteStatementInput, optFns ...func(*redshiftdata.Options)) (*redshiftdata.ExecuteStatementOutput, error)
}

// --- client bundle + classification --------------------------------------

// runClients bundles the per-service AWS clients run dispatches to. Tests
// pass fakes; production wiring constructs real ones in newRunClients.
type runClients struct {
	ECS          ecsRunAPI
	Lambda       lambdaInvokeAPI
	SFN          sfnStartAPI
	Batch        batchSubmitAPI
	Glue         glueStartAPI
	CodeBuild    codebuildStartAPI
	CodePipeline codepipelineStartAPI
	SageMaker    sagemakerStartAPI
	RedshiftData redshiftDataAPI
}

// targetKind enumerates the target shapes the run subcommand can dispatch.
type targetKind int

const (
	targetKindUnsupported targetKind = iota
	targetKindECS
	targetKindLambda
	targetKindSFN
	targetKindBatch
	targetKindGlue
	targetKindCodeBuild
	targetKindCodePipeline
	targetKindSageMakerPipeline
	targetKindRedshiftData
)

// classifyTarget inspects t.Arn (with t.EcsParameters as a tiebreaker for
// ECS) to decide which AWS API to call. Returns targetKindUnsupported with
// a descriptive error for everything ebschedule run does not handle yet
// (SQS, SNS, Kinesis, API Destination, EC2 actions, etc.).
func classifyTarget(t *Target) (targetKind, error) {
	arn := t.Arn
	switch {
	case strings.HasPrefix(arn, "arn:aws:ecs:") && t.EcsParameters != nil:
		return targetKindECS, nil
	case strings.HasPrefix(arn, "arn:aws:lambda:") && strings.Contains(arn, ":function:"):
		return targetKindLambda, nil
	case strings.HasPrefix(arn, "arn:aws:states:") && strings.Contains(arn, ":stateMachine:"):
		return targetKindSFN, nil
	case strings.HasPrefix(arn, "arn:aws:batch:") && strings.Contains(arn, ":job-queue/"):
		return targetKindBatch, nil
	case strings.HasPrefix(arn, "arn:aws:glue:") && strings.Contains(arn, ":job/"):
		return targetKindGlue, nil
	case strings.HasPrefix(arn, "arn:aws:codebuild:") && strings.Contains(arn, ":project/"):
		return targetKindCodeBuild, nil
	case strings.HasPrefix(arn, "arn:aws:codepipeline:"):
		return targetKindCodePipeline, nil
	case strings.HasPrefix(arn, "arn:aws:sagemaker:") && strings.Contains(arn, ":pipeline/"):
		return targetKindSageMakerPipeline, nil
	case strings.HasPrefix(arn, "arn:aws:redshift:") && strings.Contains(arn, ":cluster:"),
		strings.HasPrefix(arn, "arn:aws:redshift-serverless:") && strings.Contains(arn, ":workgroup/"):
		return targetKindRedshiftData, nil
	default:
		return targetKindUnsupported, fmt.Errorf("run: target %q (arn=%q) is not a supported invocation type (ECS RunTask / Lambda Invoke / Step Functions StartExecution / Batch SubmitJob / Glue StartJobRun / CodeBuild StartBuild / CodePipeline StartPipelineExecution / SageMaker Pipeline StartPipelineExecution / Redshift Data ExecuteStatement)", t.ID, arn)
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

// --- subcommand entry / dispatch -----------------------------------------

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
	case targetKindBatch:
		return runBatchTarget(ctx, out, cli.Batch, r, t, dryRun)
	case targetKindGlue:
		return runGlueTarget(ctx, out, cli.Glue, t, dryRun)
	case targetKindCodeBuild:
		return runCodeBuildTarget(ctx, out, cli.CodeBuild, t, dryRun)
	case targetKindCodePipeline:
		return runCodePipelineTarget(ctx, out, cli.CodePipeline, t, dryRun)
	case targetKindSageMakerPipeline:
		return runSageMakerTarget(ctx, out, cli.SageMaker, r, t, dryRun)
	case targetKindRedshiftData:
		return runRedshiftDataTarget(ctx, out, cli.RedshiftData, t, dryRun)
	default:
		return fmt.Errorf("run: classifier returned unknown kind for target %q", t.ID)
	}
}

// --- ECS dispatch --------------------------------------------------------

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

// --- Lambda + SFN dispatch -----------------------------------------------

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

// --- Tier A dispatch (job-runner targets) --------------------------------

// arnTrailingPath returns the substring after the last `/` in arn. Useful
// for ARNs of the form `arn:aws:<svc>:<region>:<account>:<type>/<name>`
// where the leaf name lives after a single `/`. Falls back to the input.
func arnTrailingPath(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// arnTrailingColon parses `arn:aws:<svc>:...:<type>:<name>` (e.g.
// `arn:aws:redshift:...:cluster:my-cluster`). Splits at the LAST `:` and
// returns the name segment. Falls back to the input.
func arnTrailingColon(arn string) string {
	if i := strings.LastIndex(arn, ":"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// runBatchTarget calls batch:SubmitJob. The target ARN is the job-queue;
// jobDefinition + jobName / arraySize / retryAttempts come from the rule's
// batchParameters. JobName defaults to "<rule>-<unix-ms>" when omitted so
// repeated invocations don't collide on Batch's name-uniqueness window.
func runBatchTarget(ctx context.Context, out io.Writer, cli batchSubmitAPI, r *Rule, t *Target, dryRun bool) error {
	bp := t.BatchParameters
	if bp == nil || bp.JobDefinition == "" {
		return fmt.Errorf("batch:SubmitJob requires batchParameters.jobDefinition on the target")
	}
	jobName := bp.JobName
	if jobName == "" {
		jobName = fmt.Sprintf("ebschedule-run-%s-%d", r.Name, time.Now().UnixMilli())
	}
	if dryRun {
		fmt.Fprintf(out, "[dry-run] batch:SubmitJob queue=%s jobDefinition=%s jobName=%s\n",
			t.Arn, bp.JobDefinition, jobName)
		return nil
	}
	in := &batch.SubmitJobInput{
		JobName:       aws.String(jobName),
		JobQueue:      aws.String(t.Arn),
		JobDefinition: aws.String(bp.JobDefinition),
	}
	if bp.ArraySize > 0 {
		in.ArrayProperties = &batchtypes.ArrayProperties{Size: aws.Int32(bp.ArraySize)}
	}
	if bp.RetryAttempts > 0 {
		in.RetryStrategy = &batchtypes.RetryStrategy{Attempts: aws.Int32(bp.RetryAttempts)}
	}
	resp, err := cli.SubmitJob(ctx, in)
	if err != nil {
		return fmt.Errorf("batch:SubmitJob: %w", err)
	}
	fmt.Fprintf(out, "batch:SubmitJob jobId=%s jobName=%s\n",
		aws.ToString(resp.JobId), aws.ToString(resp.JobName))
	return nil
}

// runGlueTarget calls glue:StartJobRun. The target ARN is the Glue job
// (`arn:aws:glue:region:account:job/<name>`); the job name is parsed out
// for the SDK input. We don't pass Arguments — our schema doesn't model
// per-run Glue arguments and the EventBridge wiring usually leaves them
// implicit too.
func runGlueTarget(ctx context.Context, out io.Writer, cli glueStartAPI, t *Target, dryRun bool) error {
	jobName := arnTrailingPath(t.Arn)
	if dryRun {
		fmt.Fprintf(out, "[dry-run] glue:StartJobRun jobName=%s\n", jobName)
		return nil
	}
	resp, err := cli.StartJobRun(ctx, &glue.StartJobRunInput{
		JobName: aws.String(jobName),
	})
	if err != nil {
		return fmt.Errorf("glue:StartJobRun: %w", err)
	}
	fmt.Fprintf(out, "glue:StartJobRun jobRunId=%s\n", aws.ToString(resp.JobRunId))
	return nil
}

// runCodeBuildTarget calls codebuild:StartBuild. The target ARN is the
// CodeBuild project (`arn:aws:codebuild:region:account:project/<name>`);
// the project name is parsed out for the SDK input.
func runCodeBuildTarget(ctx context.Context, out io.Writer, cli codebuildStartAPI, t *Target, dryRun bool) error {
	project := arnTrailingPath(t.Arn)
	if dryRun {
		fmt.Fprintf(out, "[dry-run] codebuild:StartBuild project=%s\n", project)
		return nil
	}
	resp, err := cli.StartBuild(ctx, &codebuild.StartBuildInput{
		ProjectName: aws.String(project),
	})
	if err != nil {
		return fmt.Errorf("codebuild:StartBuild: %w", err)
	}
	id := ""
	if resp.Build != nil {
		id = aws.ToString(resp.Build.Id)
	}
	fmt.Fprintf(out, "codebuild:StartBuild buildId=%s\n", id)
	return nil
}

// runCodePipelineTarget calls codepipeline:StartPipelineExecution. The
// target ARN's trailing segment is the pipeline name
// (`arn:aws:codepipeline:region:account:<pipeline-name>`).
func runCodePipelineTarget(ctx context.Context, out io.Writer, cli codepipelineStartAPI, t *Target, dryRun bool) error {
	// arn:aws:codepipeline:region:account:<pipeline-name> — pipeline
	// name is the last colon-delimited segment.
	pipeline := arnTrailingColon(t.Arn)
	if dryRun {
		fmt.Fprintf(out, "[dry-run] codepipeline:StartPipelineExecution pipeline=%s\n", pipeline)
		return nil
	}
	resp, err := cli.StartPipelineExecution(ctx, &codepipeline.StartPipelineExecutionInput{
		Name: aws.String(pipeline),
	})
	if err != nil {
		return fmt.Errorf("codepipeline:StartPipelineExecution: %w", err)
	}
	fmt.Fprintf(out, "codepipeline:StartPipelineExecution executionId=%s\n",
		aws.ToString(resp.PipelineExecutionId))
	return nil
}

// runSageMakerTarget calls sagemaker:StartPipelineExecution. The target
// ARN is the pipeline (`arn:aws:sagemaker:region:account:pipeline/<name>`);
// the pipeline name is parsed out. PipelineParameters from the target's
// sageMakerPipelineParameters are passed straight through. The execution
// name embeds the rule name + a millisecond timestamp so repeated `run`
// invocations don't collide on SageMaker's name-uniqueness window.
func runSageMakerTarget(ctx context.Context, out io.Writer, cli sagemakerStartAPI, r *Rule, t *Target, dryRun bool) error {
	pipeline := arnTrailingPath(t.Arn)
	name := fmt.Sprintf("ebs-run-%s-%d", r.Name, time.Now().UnixMilli())
	var params []sagemakertypes.Parameter
	if t.SageMakerPipelineParameters != nil {
		for _, p := range t.SageMakerPipelineParameters.PipelineParameterList {
			params = append(params, sagemakertypes.Parameter{
				Name:  aws.String(p.Name),
				Value: aws.String(p.Value),
			})
		}
	}
	if dryRun {
		fmt.Fprintf(out, "[dry-run] sagemaker:StartPipelineExecution pipeline=%s name=%s parameters=%d\n",
			pipeline, name, len(params))
		return nil
	}
	resp, err := cli.StartPipelineExecution(ctx, &sagemaker.StartPipelineExecutionInput{
		PipelineName:                 aws.String(pipeline),
		PipelineExecutionDisplayName: aws.String(name),
		PipelineParameters:           params,
	})
	if err != nil {
		return fmt.Errorf("sagemaker:StartPipelineExecution: %w", err)
	}
	fmt.Fprintf(out, "sagemaker:StartPipelineExecution arn=%s\n",
		aws.ToString(resp.PipelineExecutionArn))
	return nil
}

// runRedshiftDataTarget calls redshift-data:ExecuteStatement. Cluster vs
// serverless is detected from the ARN prefix; database / dbUser /
// secretManagerArn / sql / statementName come from the rule's
// redshiftDataParameters. If RedshiftDataParameters.Sqls is set with
// multiple statements, this falls back to the first; multi-statement
// support would call BatchExecuteStatement instead, deferred for now.
func runRedshiftDataTarget(ctx context.Context, out io.Writer, cli redshiftDataAPI, t *Target, dryRun bool) error {
	p := t.RedshiftDataParameters
	if p == nil || p.Database == "" {
		return fmt.Errorf("redshift-data:ExecuteStatement requires redshiftDataParameters.database on the target")
	}
	sqlText := p.Sql
	if sqlText == "" && len(p.Sqls) > 0 {
		sqlText = p.Sqls[0]
	}
	if sqlText == "" {
		return fmt.Errorf("redshift-data:ExecuteStatement requires redshiftDataParameters.sql or .sqls")
	}
	in := &redshiftdata.ExecuteStatementInput{
		Database:      aws.String(p.Database),
		Sql:           aws.String(sqlText),
		StatementName: aws.String(p.StatementName),
	}
	if p.DbUser != "" {
		in.DbUser = aws.String(p.DbUser)
	}
	if p.SecretManagerArn != "" {
		in.SecretArn = aws.String(p.SecretManagerArn)
	}
	target := ""
	switch {
	case strings.HasPrefix(t.Arn, "arn:aws:redshift:"):
		// arn:aws:redshift:region:account:cluster:<name>
		in.ClusterIdentifier = aws.String(arnTrailingColon(t.Arn))
		target = "cluster=" + aws.ToString(in.ClusterIdentifier)
	case strings.HasPrefix(t.Arn, "arn:aws:redshift-serverless:"):
		// arn:aws:redshift-serverless:region:account:workgroup/<name>
		in.WorkgroupName = aws.String(arnTrailingPath(t.Arn))
		target = "workgroup=" + aws.ToString(in.WorkgroupName)
	}
	if dryRun {
		fmt.Fprintf(out, "[dry-run] redshift-data:ExecuteStatement %s database=%s sqlBytes=%d\n",
			target, p.Database, len(sqlText))
		return nil
	}
	resp, err := cli.ExecuteStatement(ctx, in)
	if err != nil {
		return fmt.Errorf("redshift-data:ExecuteStatement: %w", err)
	}
	fmt.Fprintf(out, "redshift-data:ExecuteStatement statementId=%s\n",
		aws.ToString(resp.Id))
	return nil
}

// --- AWS clients ---------------------------------------------------------

// newRunClients builds the AWS service clients used by the run subcommand.
// Real AWS wiring; tests skip this and construct runClients directly.
func newRunClients(ctx context.Context, region string) (*runClients, error) {
	awsCfg, err := loadAWS(ctx, region)
	if err != nil {
		return nil, err
	}
	return &runClients{
		ECS:          ecs.NewFromConfig(awsCfg),
		Lambda:       lambda.NewFromConfig(awsCfg),
		SFN:          sfn.NewFromConfig(awsCfg),
		Batch:        batch.NewFromConfig(awsCfg),
		Glue:         glue.NewFromConfig(awsCfg),
		CodeBuild:    codebuild.NewFromConfig(awsCfg),
		CodePipeline: codepipeline.NewFromConfig(awsCfg),
		SageMaker:    sagemaker.NewFromConfig(awsCfg),
		RedshiftData: redshiftdata.NewFromConfig(awsCfg),
	}, nil
}

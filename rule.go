package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

// ebAPI is the subset of *eventbridge.Client that ebschedule actually uses.
// Defining it here lets tests inject a fake without standing up a real
// AWS SDK client. *eventbridge.Client satisfies this interface implicitly.
type ebAPI interface {
	ListRules(ctx context.Context, in *eventbridge.ListRulesInput, optFns ...func(*eventbridge.Options)) (*eventbridge.ListRulesOutput, error)
	ListTargetsByRule(ctx context.Context, in *eventbridge.ListTargetsByRuleInput, optFns ...func(*eventbridge.Options)) (*eventbridge.ListTargetsByRuleOutput, error)
	ListTagsForResource(ctx context.Context, in *eventbridge.ListTagsForResourceInput, optFns ...func(*eventbridge.Options)) (*eventbridge.ListTagsForResourceOutput, error)
	DescribeRule(ctx context.Context, in *eventbridge.DescribeRuleInput, optFns ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error)
	PutRule(ctx context.Context, in *eventbridge.PutRuleInput, optFns ...func(*eventbridge.Options)) (*eventbridge.PutRuleOutput, error)
	PutTargets(ctx context.Context, in *eventbridge.PutTargetsInput, optFns ...func(*eventbridge.Options)) (*eventbridge.PutTargetsOutput, error)
	RemoveTargets(ctx context.Context, in *eventbridge.RemoveTargetsInput, optFns ...func(*eventbridge.Options)) (*eventbridge.RemoveTargetsOutput, error)
	TagResource(ctx context.Context, in *eventbridge.TagResourceInput, optFns ...func(*eventbridge.Options)) (*eventbridge.TagResourceOutput, error)
	UntagResource(ctx context.Context, in *eventbridge.UntagResourceInput, optFns ...func(*eventbridge.Options)) (*eventbridge.UntagResourceOutput, error)
	DeleteRule(ctx context.Context, in *eventbridge.DeleteRuleInput, optFns ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error)
}

// --- types -----------------------------------------------------------------

type Rule struct {
	Name               string            `yaml:"name"`
	Description        string            `yaml:"description,omitempty"`
	ScheduleExpression string            `yaml:"scheduleExpression,omitempty"`
	EventPattern       jsonField         `yaml:"eventPattern,omitempty"`
	State              string            `yaml:"state,omitempty"` // ENABLED | DISABLED
	RoleArn            string            `yaml:"roleArn,omitempty"`
	Tags               map[string]string `yaml:"tags,omitempty"`
	Targets            []*Target         `yaml:"targets"`
}

type Target struct {
	ID                          string                       `yaml:"id"`
	Arn                         string                       `yaml:"arn"`
	RoleArn                     string                       `yaml:"roleArn,omitempty"`
	Input                       jsonField                    `yaml:"input,omitempty"`
	InputPath                   string                       `yaml:"inputPath,omitempty"`
	InputTransformer            *InputTransformer            `yaml:"inputTransformer,omitempty"`
	RetryPolicy                 *RetryPolicy                 `yaml:"retryPolicy,omitempty"`
	DeadLetterConfig            *DeadLetterConfig            `yaml:"deadLetterConfig,omitempty"`
	EcsParameters               *RuleEcsParameters           `yaml:"ecsParameters,omitempty"`
	SqsParameters               *SqsParameters               `yaml:"sqsParameters,omitempty"`
	KinesisParameters           *RuleKinesisParameters       `yaml:"kinesisParameters,omitempty"`
	BatchParameters             *BatchParameters             `yaml:"batchParameters,omitempty"`
	RedshiftDataParameters      *RedshiftDataParameters      `yaml:"redshiftDataParameters,omitempty"`
	SageMakerPipelineParameters *SageMakerPipelineParameters `yaml:"sageMakerPipelineParameters,omitempty"`
	HttpParameters              *HttpParameters              `yaml:"httpParameters,omitempty"`
}

type InputTransformer struct {
	InputPathsMap map[string]string `yaml:"inputPathsMap,omitempty"`
	InputTemplate string            `yaml:"inputTemplate"`
}

type RuleEcsParameters struct {
	TaskDefinitionArn string   `yaml:"taskDefinitionArn"`
	TaskCount         int32    `yaml:"taskCount,omitempty"`
	LaunchType        string   `yaml:"launchType,omitempty"`
	PlatformVersion   string   `yaml:"platformVersion,omitempty"`
	Subnets           []string `yaml:"subnets,omitempty"`
	SecurityGroups    []string `yaml:"securityGroups,omitempty"`
	AssignPublicIp    string   `yaml:"assignPublicIp,omitempty"`
	Group             string   `yaml:"group,omitempty"`
	PropagateTags     string   `yaml:"propagateTags,omitempty"` // TASK_DEFINITION
}

// RuleKinesisParameters is the EventBridge Rule shape: a JSON path used to
// pull the partition key from the event payload (Scheduler uses a literal
// PartitionKey instead).
type RuleKinesisParameters struct {
	PartitionKeyPath string `yaml:"partitionKeyPath"`
}

// BatchParameters launches an AWS Batch job. ArraySize / RetryAttempts map
// to nested ArrayProperties / RetryStrategy in the SDK type.
type BatchParameters struct {
	JobDefinition string `yaml:"jobDefinition"`
	JobName       string `yaml:"jobName"`
	ArraySize     int32  `yaml:"arraySize,omitempty"`
	RetryAttempts int32  `yaml:"retryAttempts,omitempty"`
}

// RedshiftDataParameters runs a SQL statement via the Redshift Data API.
// Auth is via DbUser (IAM) or SecretManagerArn (Secrets Manager).
type RedshiftDataParameters struct {
	Database         string   `yaml:"database"`
	DbUser           string   `yaml:"dbUser,omitempty"`
	SecretManagerArn string   `yaml:"secretManagerArn,omitempty"`
	Sql              string   `yaml:"sql,omitempty"`
	Sqls             []string `yaml:"sqls,omitempty"`
	StatementName    string   `yaml:"statementName,omitempty"`
	WithEvent        bool     `yaml:"withEvent,omitempty"`
}

// HttpParameters configures an EventBridge API Destination invocation.
type HttpParameters struct {
	HeaderParameters      map[string]string `yaml:"headerParameters,omitempty"`
	PathParameterValues   []string          `yaml:"pathParameterValues,omitempty"`
	QueryStringParameters map[string]string `yaml:"queryStringParameters,omitempty"`
}

// --- client ----------------------------------------------------------------

func newEBClient(ctx context.Context, region string) (*eventbridge.Client, error) {
	awsCfg, err := loadAWS(ctx, region)
	if err != nil {
		return nil, err
	}
	return eventbridge.NewFromConfig(awsCfg), nil
}

// --- dump ------------------------------------------------------------------

func dumpRules(ctx context.Context, region, bus, prefix string) ([]*Rule, error) {
	return dumpRulesFiltered(ctx, region, bus, prefix, nil)
}

// dumpRulesFiltered behaves like dumpRules but drops Rules that don't
// carry every tag in tagFilter. nil/empty tagFilter means "no filter".
func dumpRulesFiltered(ctx context.Context, region, bus, prefix string, tagFilter map[string]string) ([]*Rule, error) {
	cli, err := newEBClient(ctx, region)
	if err != nil {
		return nil, err
	}
	return dumpRulesWith(ctx, cli, bus, prefix, tagFilter)
}

// matchesTagFilter reports whether tags carries every key/value in filter.
// Empty / nil filter trivially matches everything.
func matchesTagFilter(tags, filter map[string]string) bool {
	for k, v := range filter {
		if tags[k] != v {
			return false
		}
	}
	return true
}

func dumpRulesWith(ctx context.Context, cli ebAPI, bus, prefix string, tagFilter map[string]string) ([]*Rule, error) {
	var out []*Rule
	var token *string
	for {
		in := &eventbridge.ListRulesInput{
			EventBusName: aws.String(bus),
			NextToken:    token,
		}
		if prefix != "" {
			in.NamePrefix = aws.String(prefix)
		}
		resp, err := cli.ListRules(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, r := range resp.Rules {
			arn := aws.ToString(r.Arn)
			name := aws.ToString(r.Name)
			tags, err := listRuleTags(ctx, cli, arn)
			if err != nil {
				return nil, err
			}
			if !matchesTagFilter(tags, tagFilter) {
				continue
			}
			rule := fromRemoteRule(r)
			tgts, err := listRuleTargets(ctx, cli, bus, name)
			if err != nil {
				return nil, err
			}
			rule.Targets = tgts
			delete(tags, trackingTagKey)
			if len(tags) > 0 {
				rule.Tags = tags
			}
			out = append(out, rule)
		}
		if aws.ToString(resp.NextToken) == "" {
			break
		}
		token = resp.NextToken
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// canonicalizeRule returns a copy of r prepared for diff/apply YAML
// comparison: cfg.Tags merged in, Targets sorted by ID so target ordering
// in the user's YAML doesn't surface as drift (listRuleTargets sorts the
// remote side identically). Non-destructive.
func canonicalizeRule(r *Rule, cfg *Config) *Rule {
	out := *r
	out.Tags = mergeTags(cfg.Tags, r.Tags)
	if len(out.Targets) > 1 {
		sorted := make([]*Target, len(out.Targets))
		copy(sorted, out.Targets)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
		out.Targets = sorted
	}
	return &out
}

func fromRemoteRule(r ebtypes.Rule) *Rule {
	return &Rule{
		Name:               aws.ToString(r.Name),
		Description:        aws.ToString(r.Description),
		ScheduleExpression: aws.ToString(r.ScheduleExpression),
		EventPattern:       jsonFieldFromAWS(aws.ToString(r.EventPattern)),
		State:              string(r.State),
		RoleArn:            aws.ToString(r.RoleArn),
	}
}

func listRuleTargets(ctx context.Context, cli ebAPI, bus, ruleName string) ([]*Target, error) {
	resp, err := cli.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{
		Rule:         aws.String(ruleName),
		EventBusName: aws.String(bus),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Target, 0, len(resp.Targets))
	for _, t := range resp.Targets {
		out = append(out, fromRemoteTarget(t))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func listRuleTags(ctx context.Context, cli ebAPI, ruleArn string) (map[string]string, error) {
	resp, err := cli.ListTagsForResource(ctx, &eventbridge.ListTagsForResourceInput{
		ResourceARN: aws.String(ruleArn),
	})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, t := range resp.Tags {
		out[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return out, nil
}

func fromRemoteTarget(t ebtypes.Target) *Target {
	tgt := &Target{
		ID:        aws.ToString(t.Id),
		Arn:       aws.ToString(t.Arn),
		RoleArn:   aws.ToString(t.RoleArn),
		Input:     jsonFieldFromAWS(aws.ToString(t.Input)),
		InputPath: aws.ToString(t.InputPath),
	}
	if t.InputTransformer != nil {
		tgt.InputTransformer = &InputTransformer{
			InputPathsMap: t.InputTransformer.InputPathsMap,
			InputTemplate: aws.ToString(t.InputTransformer.InputTemplate),
		}
	}
	if t.RetryPolicy != nil {
		tgt.RetryPolicy = &RetryPolicy{
			MaximumRetryAttempts:     aws.ToInt32(t.RetryPolicy.MaximumRetryAttempts),
			MaximumEventAgeInSeconds: aws.ToInt32(t.RetryPolicy.MaximumEventAgeInSeconds),
		}
	}
	if t.DeadLetterConfig != nil {
		tgt.DeadLetterConfig = &DeadLetterConfig{Arn: aws.ToString(t.DeadLetterConfig.Arn)}
	}
	if t.EcsParameters != nil {
		ep := &RuleEcsParameters{
			TaskDefinitionArn: aws.ToString(t.EcsParameters.TaskDefinitionArn),
			TaskCount:         aws.ToInt32(t.EcsParameters.TaskCount),
			LaunchType:        string(t.EcsParameters.LaunchType),
			PlatformVersion:   aws.ToString(t.EcsParameters.PlatformVersion),
			Group:             aws.ToString(t.EcsParameters.Group),
			PropagateTags:     string(t.EcsParameters.PropagateTags),
		}
		if nc := t.EcsParameters.NetworkConfiguration; nc != nil && nc.AwsvpcConfiguration != nil {
			ep.Subnets = nc.AwsvpcConfiguration.Subnets
			ep.SecurityGroups = nc.AwsvpcConfiguration.SecurityGroups
			ep.AssignPublicIp = string(nc.AwsvpcConfiguration.AssignPublicIp)
		}
		tgt.EcsParameters = ep
	}
	if t.SqsParameters != nil {
		tgt.SqsParameters = &SqsParameters{MessageGroupId: aws.ToString(t.SqsParameters.MessageGroupId)}
	}
	if t.KinesisParameters != nil {
		tgt.KinesisParameters = &RuleKinesisParameters{
			PartitionKeyPath: aws.ToString(t.KinesisParameters.PartitionKeyPath),
		}
	}
	if t.BatchParameters != nil {
		bp := &BatchParameters{
			JobDefinition: aws.ToString(t.BatchParameters.JobDefinition),
			JobName:       aws.ToString(t.BatchParameters.JobName),
		}
		if t.BatchParameters.ArrayProperties != nil {
			bp.ArraySize = t.BatchParameters.ArrayProperties.Size
		}
		if t.BatchParameters.RetryStrategy != nil {
			bp.RetryAttempts = t.BatchParameters.RetryStrategy.Attempts
		}
		tgt.BatchParameters = bp
	}
	if t.RedshiftDataParameters != nil {
		tgt.RedshiftDataParameters = &RedshiftDataParameters{
			Database:         aws.ToString(t.RedshiftDataParameters.Database),
			DbUser:           aws.ToString(t.RedshiftDataParameters.DbUser),
			SecretManagerArn: aws.ToString(t.RedshiftDataParameters.SecretManagerArn),
			Sql:              aws.ToString(t.RedshiftDataParameters.Sql),
			Sqls:             t.RedshiftDataParameters.Sqls,
			StatementName:    aws.ToString(t.RedshiftDataParameters.StatementName),
			WithEvent:        t.RedshiftDataParameters.WithEvent,
		}
	}
	if t.SageMakerPipelineParameters != nil {
		smp := &SageMakerPipelineParameters{}
		for _, p := range t.SageMakerPipelineParameters.PipelineParameterList {
			smp.PipelineParameterList = append(smp.PipelineParameterList, SageMakerPipelineParameter{
				Name:  aws.ToString(p.Name),
				Value: aws.ToString(p.Value),
			})
		}
		tgt.SageMakerPipelineParameters = smp
	}
	if t.HttpParameters != nil {
		tgt.HttpParameters = &HttpParameters{
			HeaderParameters:      t.HttpParameters.HeaderParameters,
			PathParameterValues:   t.HttpParameters.PathParameterValues,
			QueryStringParameters: t.HttpParameters.QueryStringParameters,
		}
	}
	return tgt
}

// --- diff ------------------------------------------------------------------

// diffRules emits a unified diff per rule to out and returns whether any
// rule has drift. Drift = a desired rule is missing remotely or differs.
func diffRules(ctx context.Context, out io.Writer, cfg *Config) (bool, error) {
	current, err := dumpRules(ctx, cfg.Region, cfg.bus(), "")
	if err != nil {
		return false, err
	}
	cur := map[string]*Rule{}
	for _, r := range current {
		cur[r.Name] = r
	}
	drift := false
	for _, want := range cfg.Rules {
		desiredYAML := mustYAML(canonicalizeRule(want, cfg))
		got, ok := cur[want.Name]
		if !ok {
			fmt.Fprint(out, unifiedDiff("rule:"+want.Name, "", desiredYAML))
			drift = true
			continue
		}
		gotYAML := mustYAML(got)
		if gotYAML == desiredYAML {
			continue
		}
		fmt.Fprint(out, unifiedDiff("rule:"+want.Name, gotYAML, desiredYAML))
		drift = true
	}
	return drift, nil
}

// --- apply -----------------------------------------------------------------

func applyRules(ctx context.Context, out io.Writer, cfg *Config, dryRun, prune bool) error {
	cli, err := newEBClient(ctx, cfg.Region)
	if err != nil {
		return err
	}
	return applyRulesWith(ctx, out, cli, cfg, dryRun, prune)
}

func applyRulesWith(ctx context.Context, out io.Writer, cli ebAPI, cfg *Config, dryRun, prune bool) error {
	bus := cfg.bus()
	desired := map[string]bool{}
	for _, r := range cfg.Rules {
		desired[r.Name] = true
		if err := applyOneRule(ctx, out, cli, bus, cfg, r, dryRun); err != nil {
			return fmt.Errorf("rule %s: %w", r.Name, err)
		}
	}
	if !prune {
		return nil
	}
	if cfg.TrackingID == "" {
		return fmt.Errorf("-prune requires trackingId in config (safety guard)")
	}
	current, err := dumpRulesWith(ctx, cli, bus, "", nil)
	if err != nil {
		return err
	}
	for _, r := range current {
		if desired[r.Name] {
			continue
		}
		tracked, err := isRuleTracked(ctx, cli, bus, r.Name, cfg.TrackingID)
		if err != nil {
			return err
		}
		if !tracked {
			continue
		}
		fmt.Fprintf(out, "- rule:%s (delete)\n", r.Name)
		if dryRun {
			continue
		}
		if err := deleteRule(ctx, cli, bus, r.Name); err != nil {
			return err
		}
	}
	return nil
}

// fetchCurrentRule returns the canonical view of a rule (with targets and
// tracking-tag-stripped tags) for diff-vs-apply comparison. exists=false
// means the rule doesn't exist yet.
func fetchCurrentRule(ctx context.Context, cli ebAPI, bus, name string) (snap *Rule, arn string, exists bool, err error) {
	desc, err := cli.DescribeRule(ctx, &eventbridge.DescribeRuleInput{
		Name: aws.String(name), EventBusName: aws.String(bus),
	})
	if err != nil {
		var nf *ebtypes.ResourceNotFoundException
		if errors.As(err, &nf) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	arn = aws.ToString(desc.Arn)
	snap = fromRemoteRule(ebtypes.Rule{
		Name:               desc.Name,
		Description:        desc.Description,
		ScheduleExpression: desc.ScheduleExpression,
		EventPattern:       desc.EventPattern,
		State:              desc.State,
		RoleArn:            desc.RoleArn,
	})
	targets, err := listRuleTargets(ctx, cli, bus, name)
	if err != nil {
		return nil, "", false, err
	}
	snap.Targets = targets
	tags, err := listRuleTags(ctx, cli, arn)
	if err != nil {
		return nil, "", false, err
	}
	delete(tags, trackingTagKey)
	if len(tags) > 0 {
		snap.Tags = tags
	}
	return snap, arn, true, nil
}

func applyOneRule(ctx context.Context, out io.Writer, cli ebAPI, bus string, cfg *Config, r *Rule, dryRun bool) error {
	current, currentArn, exists, err := fetchCurrentRule(ctx, cli, bus, r.Name)
	if err != nil {
		return err
	}
	desiredYAML := mustYAML(canonicalizeRule(r, cfg))

	switch {
	case !exists:
		fmt.Fprintf(out, "+ rule:%s (create)\n", r.Name)
	default:
		if mustYAML(current) == desiredYAML {
			fmt.Fprintf(out, "= rule:%s (no-op)\n", r.Name)
			return nil
		}
		fmt.Fprintf(out, "~ rule:%s (update)\n", r.Name)
	}
	if dryRun {
		return nil
	}

	state := ebtypes.RuleStateEnabled
	if strings.EqualFold(r.State, "DISABLED") {
		state = ebtypes.RuleStateDisabled
	}
	in := &eventbridge.PutRuleInput{
		Name:         aws.String(r.Name),
		EventBusName: aws.String(bus),
		State:        state,
	}
	if r.Description != "" {
		in.Description = aws.String(r.Description)
	}
	if r.ScheduleExpression != "" {
		in.ScheduleExpression = aws.String(r.ScheduleExpression)
	}
	if r.EventPattern != "" {
		in.EventPattern = aws.String(string(r.EventPattern))
	}
	if r.RoleArn != "" {
		in.RoleArn = aws.String(r.RoleArn)
	}
	// Tags are reconciled below; PutRule.Tags is create-only, so skipping it.
	putOut, err := cli.PutRule(ctx, in)
	if err != nil {
		return err
	}
	if currentArn == "" {
		currentArn = aws.ToString(putOut.RuleArn)
	}
	currentTagMap, err := listRuleTags(ctx, cli, currentArn)
	if err != nil {
		return err
	}
	desiredTags := mergeTags(cfg.Tags, r.Tags)
	setFn := func(tags map[string]string) error {
		awsTags := make([]ebtypes.Tag, 0, len(tags))
		for k, v := range tags {
			awsTags = append(awsTags, ebtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		_, err := cli.TagResource(ctx, &eventbridge.TagResourceInput{
			ResourceARN: aws.String(currentArn), Tags: awsTags,
		})
		return err
	}
	unsetFn := func(keys []string) error {
		_, err := cli.UntagResource(ctx, &eventbridge.UntagResourceInput{
			ResourceARN: aws.String(currentArn), TagKeys: keys,
		})
		return err
	}
	if err := reconcileTags(currentTagMap, desiredTags, cfg.TrackingID, setFn, unsetFn); err != nil {
		return err
	}
	// Sync targets.
	currentTargets, err := listRuleTargets(ctx, cli, bus, r.Name)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, t := range r.Targets {
		want[t.ID] = true
	}
	var toRemove []string
	for _, t := range currentTargets {
		if !want[t.ID] {
			toRemove = append(toRemove, t.ID)
		}
	}
	if len(toRemove) > 0 {
		if _, err := cli.RemoveTargets(ctx, &eventbridge.RemoveTargetsInput{
			Rule: aws.String(r.Name), EventBusName: aws.String(bus), Ids: toRemove,
		}); err != nil {
			return err
		}
	}
	if len(r.Targets) > 0 {
		awsTgts := make([]ebtypes.Target, 0, len(r.Targets))
		for _, t := range r.Targets {
			awsTgts = append(awsTgts, toAWSTarget(t))
		}
		if _, err := cli.PutTargets(ctx, &eventbridge.PutTargetsInput{
			Rule: aws.String(r.Name), EventBusName: aws.String(bus), Targets: awsTgts,
		}); err != nil {
			return err
		}
	}
	return nil
}

func toAWSTarget(t *Target) ebtypes.Target {
	at := ebtypes.Target{Id: aws.String(t.ID), Arn: aws.String(t.Arn)}
	if t.RoleArn != "" {
		at.RoleArn = aws.String(t.RoleArn)
	}
	if t.Input != "" {
		at.Input = aws.String(string(t.Input))
	}
	if t.InputPath != "" {
		at.InputPath = aws.String(t.InputPath)
	}
	if t.InputTransformer != nil {
		at.InputTransformer = &ebtypes.InputTransformer{
			InputTemplate: aws.String(t.InputTransformer.InputTemplate),
			InputPathsMap: t.InputTransformer.InputPathsMap,
		}
	}
	if t.RetryPolicy != nil {
		at.RetryPolicy = &ebtypes.RetryPolicy{
			MaximumRetryAttempts:     aws.Int32(t.RetryPolicy.MaximumRetryAttempts),
			MaximumEventAgeInSeconds: aws.Int32(t.RetryPolicy.MaximumEventAgeInSeconds),
		}
	}
	if t.DeadLetterConfig != nil {
		at.DeadLetterConfig = &ebtypes.DeadLetterConfig{Arn: aws.String(t.DeadLetterConfig.Arn)}
	}
	if t.EcsParameters != nil {
		ep := &ebtypes.EcsParameters{
			TaskDefinitionArn: aws.String(t.EcsParameters.TaskDefinitionArn),
			LaunchType:        ebtypes.LaunchType(t.EcsParameters.LaunchType),
		}
		if t.EcsParameters.TaskCount > 0 {
			ep.TaskCount = aws.Int32(t.EcsParameters.TaskCount)
		}
		if t.EcsParameters.PlatformVersion != "" {
			ep.PlatformVersion = aws.String(t.EcsParameters.PlatformVersion)
		}
		if t.EcsParameters.Group != "" {
			ep.Group = aws.String(t.EcsParameters.Group)
		}
		if t.EcsParameters.PropagateTags != "" {
			ep.PropagateTags = ebtypes.PropagateTags(t.EcsParameters.PropagateTags)
		}
		if len(t.EcsParameters.Subnets) > 0 {
			ep.NetworkConfiguration = &ebtypes.NetworkConfiguration{
				AwsvpcConfiguration: &ebtypes.AwsVpcConfiguration{
					Subnets:        t.EcsParameters.Subnets,
					SecurityGroups: t.EcsParameters.SecurityGroups,
					AssignPublicIp: ebtypes.AssignPublicIp(t.EcsParameters.AssignPublicIp),
				},
			}
		}
		at.EcsParameters = ep
	}
	if t.SqsParameters != nil {
		at.SqsParameters = &ebtypes.SqsParameters{
			MessageGroupId: nilIfEmpty(t.SqsParameters.MessageGroupId),
		}
	}
	if t.KinesisParameters != nil {
		at.KinesisParameters = &ebtypes.KinesisParameters{
			PartitionKeyPath: aws.String(t.KinesisParameters.PartitionKeyPath),
		}
	}
	if t.BatchParameters != nil {
		bp := &ebtypes.BatchParameters{
			JobDefinition: aws.String(t.BatchParameters.JobDefinition),
			JobName:       aws.String(t.BatchParameters.JobName),
		}
		if t.BatchParameters.ArraySize > 0 {
			bp.ArrayProperties = &ebtypes.BatchArrayProperties{Size: t.BatchParameters.ArraySize}
		}
		if t.BatchParameters.RetryAttempts > 0 {
			bp.RetryStrategy = &ebtypes.BatchRetryStrategy{Attempts: t.BatchParameters.RetryAttempts}
		}
		at.BatchParameters = bp
	}
	if t.RedshiftDataParameters != nil {
		at.RedshiftDataParameters = &ebtypes.RedshiftDataParameters{
			Database:         aws.String(t.RedshiftDataParameters.Database),
			DbUser:           nilIfEmpty(t.RedshiftDataParameters.DbUser),
			SecretManagerArn: nilIfEmpty(t.RedshiftDataParameters.SecretManagerArn),
			Sql:              nilIfEmpty(t.RedshiftDataParameters.Sql),
			Sqls:             t.RedshiftDataParameters.Sqls,
			StatementName:    nilIfEmpty(t.RedshiftDataParameters.StatementName),
			WithEvent:        t.RedshiftDataParameters.WithEvent,
		}
	}
	if t.SageMakerPipelineParameters != nil {
		smp := &ebtypes.SageMakerPipelineParameters{}
		for _, p := range t.SageMakerPipelineParameters.PipelineParameterList {
			smp.PipelineParameterList = append(smp.PipelineParameterList, ebtypes.SageMakerPipelineParameter{
				Name:  aws.String(p.Name),
				Value: aws.String(p.Value),
			})
		}
		at.SageMakerPipelineParameters = smp
	}
	if t.HttpParameters != nil {
		at.HttpParameters = &ebtypes.HttpParameters{
			HeaderParameters:      t.HttpParameters.HeaderParameters,
			PathParameterValues:   t.HttpParameters.PathParameterValues,
			QueryStringParameters: t.HttpParameters.QueryStringParameters,
		}
	}
	return at
}

func isRuleTracked(ctx context.Context, cli ebAPI, bus, name, trackingID string) (bool, error) {
	desc, err := cli.DescribeRule(ctx, &eventbridge.DescribeRuleInput{
		Name: aws.String(name), EventBusName: aws.String(bus),
	})
	if err != nil {
		return false, err
	}
	tags, err := listRuleTags(ctx, cli, aws.ToString(desc.Arn))
	if err != nil {
		return false, err
	}
	return tags[trackingTagKey] == trackingID, nil
}

func deleteRule(ctx context.Context, cli ebAPI, bus, name string) error {
	tgts, err := listRuleTargets(ctx, cli, bus, name)
	if err != nil {
		return err
	}
	if len(tgts) > 0 {
		ids := make([]string, 0, len(tgts))
		for _, t := range tgts {
			ids = append(ids, t.ID)
		}
		if _, err := cli.RemoveTargets(ctx, &eventbridge.RemoveTargetsInput{
			Rule: aws.String(name), EventBusName: aws.String(bus), Ids: ids,
		}); err != nil {
			return err
		}
	}
	_, err = cli.DeleteRule(ctx, &eventbridge.DeleteRuleInput{
		Name: aws.String(name), EventBusName: aws.String(bus),
	})
	return err
}

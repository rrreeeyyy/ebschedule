package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

// --- types -----------------------------------------------------------------

type Rule struct {
	Name               string            `yaml:"name"`
	Description        string            `yaml:"description,omitempty"`
	ScheduleExpression string            `yaml:"scheduleExpression,omitempty"`
	EventPattern       string            `yaml:"eventPattern,omitempty"`
	State              string            `yaml:"state,omitempty"` // ENABLED | DISABLED
	RoleArn            string            `yaml:"roleArn,omitempty"`
	Tags               map[string]string `yaml:"tags,omitempty"`
	Targets            []*Target         `yaml:"targets"`
}

type Target struct {
	ID               string             `yaml:"id"`
	Arn              string             `yaml:"arn"`
	RoleArn          string             `yaml:"roleArn,omitempty"`
	Input            string             `yaml:"input,omitempty"`
	InputPath        string             `yaml:"inputPath,omitempty"`
	InputTransformer *InputTransformer  `yaml:"inputTransformer,omitempty"`
	RetryPolicy      *RetryPolicy       `yaml:"retryPolicy,omitempty"`
	DeadLetterConfig *DeadLetterConfig  `yaml:"deadLetterConfig,omitempty"`
	EcsParameters    *RuleEcsParameters `yaml:"ecsParameters,omitempty"`
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
	cli, err := newEBClient(ctx, region)
	if err != nil {
		return nil, err
	}
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
			rule := fromRemoteRule(r)
			tgts, err := listRuleTargets(ctx, cli, bus, *r.Name)
			if err != nil {
				return nil, err
			}
			rule.Targets = tgts
			tags, err := listRuleTags(ctx, cli, *r.Arn)
			if err != nil {
				return nil, err
			}
			delete(tags, trackingTagKey)
			if len(tags) > 0 {
				rule.Tags = tags
			}
			out = append(out, rule)
		}
		if resp.NextToken == nil {
			break
		}
		token = resp.NextToken
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func fromRemoteRule(r ebtypes.Rule) *Rule {
	return &Rule{
		Name:               aws.ToString(r.Name),
		Description:        aws.ToString(r.Description),
		ScheduleExpression: aws.ToString(r.ScheduleExpression),
		EventPattern:       aws.ToString(r.EventPattern),
		State:              string(r.State),
		RoleArn:            aws.ToString(r.RoleArn),
	}
}

func listRuleTargets(ctx context.Context, cli *eventbridge.Client, bus, ruleName string) ([]*Target, error) {
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

func listRuleTags(ctx context.Context, cli *eventbridge.Client, ruleArn string) (map[string]string, error) {
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
		Input:     aws.ToString(t.Input),
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
	return tgt
}

// --- diff ------------------------------------------------------------------

func diffRules(ctx context.Context, cfg *Config) error {
	current, err := dumpRules(ctx, cfg.Region, cfg.bus(), "")
	if err != nil {
		return err
	}
	cur := map[string]*Rule{}
	for _, r := range current {
		cur[r.Name] = r
	}
	for _, want := range cfg.Rules {
		desired := *want
		desired.Tags = mergeTags(cfg.Tags, want.Tags)
		desiredYAML := mustYAML(&desired)
		got, ok := cur[want.Name]
		if !ok {
			fmt.Print(unifiedDiff("rule:"+want.Name, "", desiredYAML))
			continue
		}
		gotYAML := mustYAML(got)
		if gotYAML == desiredYAML {
			continue
		}
		fmt.Print(unifiedDiff("rule:"+want.Name, gotYAML, desiredYAML))
	}
	return nil
}

// --- apply -----------------------------------------------------------------

func applyRules(ctx context.Context, cfg *Config, dryRun, prune bool) error {
	bus := cfg.bus()
	cli, err := newEBClient(ctx, cfg.Region)
	if err != nil {
		return err
	}
	desired := map[string]bool{}
	for _, r := range cfg.Rules {
		desired[r.Name] = true
		if err := applyOneRule(ctx, cli, bus, cfg, r, dryRun); err != nil {
			return fmt.Errorf("rule %s: %w", r.Name, err)
		}
	}
	if !prune {
		return nil
	}
	if cfg.TrackingID == "" {
		return fmt.Errorf("-prune requires trackingId in config (safety guard)")
	}
	current, err := dumpRules(ctx, cfg.Region, bus, "")
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
		fmt.Printf("- rule:%s (delete)\n", r.Name)
		if dryRun {
			continue
		}
		if err := deleteRule(ctx, cli, bus, r.Name); err != nil {
			return err
		}
	}
	return nil
}

func applyOneRule(ctx context.Context, cli *eventbridge.Client, bus string, cfg *Config, r *Rule, dryRun bool) error {
	fmt.Printf("+ rule:%s (apply)\n", r.Name)
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
		in.EventPattern = aws.String(r.EventPattern)
	}
	if r.RoleArn != "" {
		in.RoleArn = aws.String(r.RoleArn)
	}
	// Tags are reconciled below; PutRule.Tags is create-only, so skipping it.
	if _, err := cli.PutRule(ctx, in); err != nil {
		return err
	}
	desc, err := cli.DescribeRule(ctx, &eventbridge.DescribeRuleInput{
		Name: aws.String(r.Name), EventBusName: aws.String(bus),
	})
	if err != nil {
		return err
	}
	current, err := listRuleTags(ctx, cli, *desc.Arn)
	if err != nil {
		return err
	}
	desired := mergeTags(cfg.Tags, r.Tags)
	setFn := func(tags map[string]string) error {
		awsTags := make([]ebtypes.Tag, 0, len(tags))
		for k, v := range tags {
			awsTags = append(awsTags, ebtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		_, err := cli.TagResource(ctx, &eventbridge.TagResourceInput{
			ResourceARN: desc.Arn, Tags: awsTags,
		})
		return err
	}
	unsetFn := func(keys []string) error {
		_, err := cli.UntagResource(ctx, &eventbridge.UntagResourceInput{
			ResourceARN: desc.Arn, TagKeys: keys,
		})
		return err
	}
	if err := reconcileTags(current, desired, cfg.TrackingID, setFn, unsetFn); err != nil {
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
		at.Input = aws.String(t.Input)
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
	return at
}

func isRuleTracked(ctx context.Context, cli *eventbridge.Client, bus, name, trackingID string) (bool, error) {
	desc, err := cli.DescribeRule(ctx, &eventbridge.DescribeRuleInput{
		Name: aws.String(name), EventBusName: aws.String(bus),
	})
	if err != nil {
		return false, err
	}
	tags, err := listRuleTags(ctx, cli, *desc.Arn)
	if err != nil {
		return false, err
	}
	return tags[trackingTagKey] == trackingID, nil
}

func deleteRule(ctx context.Context, cli *eventbridge.Client, bus, name string) error {
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

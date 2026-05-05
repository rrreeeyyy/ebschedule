package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schtypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
)

// --- types -----------------------------------------------------------------

type Schedule struct {
	Name                       string              `yaml:"name"`
	Description                string              `yaml:"description,omitempty"`
	ScheduleExpression         string              `yaml:"scheduleExpression"`
	ScheduleExpressionTimezone string              `yaml:"timezone,omitempty"`
	State                      string              `yaml:"state,omitempty"` // ENABLED | DISABLED
	StartDate                  string              `yaml:"startDate,omitempty"`
	EndDate                    string              `yaml:"endDate,omitempty"`
	KmsKeyArn                  string              `yaml:"kmsKeyArn,omitempty"`
	ActionAfterCompletion      string              `yaml:"actionAfterCompletion,omitempty"` // NONE | DELETE
	FlexibleTimeWindow         *FlexibleTimeWindow `yaml:"flexibleTimeWindow,omitempty"`
	Tags                       map[string]string   `yaml:"tags,omitempty"`
	Target                     *ScheduleTarget     `yaml:"target"`
}

type FlexibleTimeWindow struct {
	Mode                   string `yaml:"mode"` // OFF | FLEXIBLE
	MaximumWindowInMinutes int32  `yaml:"maximumWindowInMinutes,omitempty"`
}

type ScheduleTarget struct {
	Arn                   string                 `yaml:"arn"`
	RoleArn               string                 `yaml:"roleArn"`
	Input                 string                 `yaml:"input,omitempty"`
	DeadLetterConfig      *DeadLetterConfig      `yaml:"deadLetterConfig,omitempty"`
	RetryPolicy           *RetryPolicy           `yaml:"retryPolicy,omitempty"`
	EcsParameters         *SchedEcsParameters    `yaml:"ecsParameters,omitempty"`
	SqsParameters         *SqsParameters         `yaml:"sqsParameters,omitempty"`
	EventBridgeParameters *EventBridgeParameters `yaml:"eventBridgeParameters,omitempty"`
}

type SchedEcsParameters struct {
	TaskDefinitionArn string   `yaml:"taskDefinitionArn"`
	TaskCount         int32    `yaml:"taskCount,omitempty"`
	LaunchType        string   `yaml:"launchType,omitempty"`
	PlatformVersion   string   `yaml:"platformVersion,omitempty"`
	Subnets           []string `yaml:"subnets,omitempty"`
	SecurityGroups    []string `yaml:"securityGroups,omitempty"`
	AssignPublicIp    string   `yaml:"assignPublicIp,omitempty"`
}

type SqsParameters struct {
	MessageGroupId string `yaml:"messageGroupId,omitempty"`
}

type EventBridgeParameters struct {
	DetailType string `yaml:"detailType"`
	Source     string `yaml:"source"`
}

// --- client ----------------------------------------------------------------

func newSchedClient(ctx context.Context, region string) (*scheduler.Client, error) {
	awsCfg, err := loadAWS(ctx, region)
	if err != nil {
		return nil, err
	}
	return scheduler.NewFromConfig(awsCfg), nil
}

// --- dump ------------------------------------------------------------------

func dumpSchedules(ctx context.Context, region, group, prefix string) ([]*Schedule, error) {
	cli, err := newSchedClient(ctx, region)
	if err != nil {
		return nil, err
	}
	var out []*Schedule
	var token *string
	for {
		in := &scheduler.ListSchedulesInput{
			GroupName: aws.String(group),
			NextToken: token,
		}
		if prefix != "" {
			in.NamePrefix = aws.String(prefix)
		}
		resp, err := cli.ListSchedules(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, s := range resp.Schedules {
			full, err := cli.GetSchedule(ctx, &scheduler.GetScheduleInput{
				GroupName: s.GroupName, Name: s.Name,
			})
			if err != nil {
				return nil, err
			}
			sched := fromRemoteSchedule(full)
			tags, err := listSchedTags(ctx, cli, *full.Arn)
			if err != nil {
				return nil, err
			}
			delete(tags, trackingTagKey)
			if len(tags) > 0 {
				sched.Tags = tags
			}
			out = append(out, sched)
		}
		if resp.NextToken == nil {
			break
		}
		token = resp.NextToken
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func fromRemoteSchedule(s *scheduler.GetScheduleOutput) *Schedule {
	out := &Schedule{
		Name:                       aws.ToString(s.Name),
		Description:                aws.ToString(s.Description),
		ScheduleExpression:         aws.ToString(s.ScheduleExpression),
		ScheduleExpressionTimezone: aws.ToString(s.ScheduleExpressionTimezone),
		State:                      string(s.State),
		StartDate:                  formatTime(s.StartDate),
		EndDate:                    formatTime(s.EndDate),
		KmsKeyArn:                  aws.ToString(s.KmsKeyArn),
		ActionAfterCompletion:      string(s.ActionAfterCompletion),
	}
	if s.FlexibleTimeWindow != nil {
		out.FlexibleTimeWindow = &FlexibleTimeWindow{
			Mode:                   string(s.FlexibleTimeWindow.Mode),
			MaximumWindowInMinutes: aws.ToInt32(s.FlexibleTimeWindow.MaximumWindowInMinutes),
		}
	}
	if s.Target != nil {
		out.Target = fromRemoteSchedTarget(s.Target)
	}
	return out
}

func fromRemoteSchedTarget(t *schtypes.Target) *ScheduleTarget {
	st := &ScheduleTarget{
		Arn:     aws.ToString(t.Arn),
		RoleArn: aws.ToString(t.RoleArn),
		Input:   aws.ToString(t.Input),
	}
	if t.DeadLetterConfig != nil {
		st.DeadLetterConfig = &DeadLetterConfig{Arn: aws.ToString(t.DeadLetterConfig.Arn)}
	}
	if t.RetryPolicy != nil {
		st.RetryPolicy = &RetryPolicy{
			MaximumRetryAttempts:     aws.ToInt32(t.RetryPolicy.MaximumRetryAttempts),
			MaximumEventAgeInSeconds: aws.ToInt32(t.RetryPolicy.MaximumEventAgeInSeconds),
		}
	}
	if t.EcsParameters != nil {
		ep := &SchedEcsParameters{
			TaskDefinitionArn: aws.ToString(t.EcsParameters.TaskDefinitionArn),
			TaskCount:         aws.ToInt32(t.EcsParameters.TaskCount),
			LaunchType:        string(t.EcsParameters.LaunchType),
			PlatformVersion:   aws.ToString(t.EcsParameters.PlatformVersion),
		}
		if nc := t.EcsParameters.NetworkConfiguration; nc != nil && nc.AwsvpcConfiguration != nil {
			ep.Subnets = nc.AwsvpcConfiguration.Subnets
			ep.SecurityGroups = nc.AwsvpcConfiguration.SecurityGroups
			ep.AssignPublicIp = string(nc.AwsvpcConfiguration.AssignPublicIp)
		}
		st.EcsParameters = ep
	}
	if t.SqsParameters != nil {
		st.SqsParameters = &SqsParameters{MessageGroupId: aws.ToString(t.SqsParameters.MessageGroupId)}
	}
	if t.EventBridgeParameters != nil {
		st.EventBridgeParameters = &EventBridgeParameters{
			DetailType: aws.ToString(t.EventBridgeParameters.DetailType),
			Source:     aws.ToString(t.EventBridgeParameters.Source),
		}
	}
	return st
}

func listSchedTags(ctx context.Context, cli *scheduler.Client, arn string) (map[string]string, error) {
	resp, err := cli.ListTagsForResource(ctx, &scheduler.ListTagsForResourceInput{
		ResourceArn: aws.String(arn),
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

// --- diff ------------------------------------------------------------------

func diffSchedules(ctx context.Context, cfg *Config) error {
	current, err := dumpSchedules(ctx, cfg.Region, cfg.group(), "")
	if err != nil {
		return err
	}
	cur := map[string]*Schedule{}
	for _, s := range current {
		cur[s.Name] = s
	}
	for _, want := range cfg.Schedules {
		desired := *want
		desired.Tags = mergeTags(cfg.Tags, want.Tags)
		desiredYAML := mustYAML(&desired)
		got, ok := cur[want.Name]
		if !ok {
			fmt.Print(unifiedDiff("schedule:"+want.Name, "", desiredYAML))
			continue
		}
		gotYAML := mustYAML(got)
		if gotYAML == desiredYAML {
			continue
		}
		fmt.Print(unifiedDiff("schedule:"+want.Name, gotYAML, desiredYAML))
	}
	return nil
}

// --- apply -----------------------------------------------------------------

func applySchedules(ctx context.Context, cfg *Config, dryRun, prune bool) error {
	group := cfg.group()
	cli, err := newSchedClient(ctx, cfg.Region)
	if err != nil {
		return err
	}
	if err := ensureScheduleGroup(ctx, cli, group, cfg, dryRun); err != nil {
		return fmt.Errorf("ensure schedule group %s: %w", group, err)
	}
	desired := map[string]bool{}
	for _, s := range cfg.Schedules {
		desired[s.Name] = true
		if err := applyOneSchedule(ctx, cli, group, cfg, s, dryRun); err != nil {
			return fmt.Errorf("schedule %s: %w", s.Name, err)
		}
	}
	if !prune {
		return nil
	}
	if cfg.TrackingID == "" {
		return fmt.Errorf("-prune requires trackingId in config (safety guard)")
	}
	current, err := dumpSchedules(ctx, cfg.Region, group, "")
	if err != nil {
		return err
	}
	for _, s := range current {
		if desired[s.Name] {
			continue
		}
		tracked, err := isSchedTracked(ctx, cli, group, s.Name, cfg.TrackingID)
		if err != nil {
			return err
		}
		if !tracked {
			continue
		}
		fmt.Printf("- schedule:%s (delete)\n", s.Name)
		if dryRun {
			continue
		}
		if _, err := cli.DeleteSchedule(ctx, &scheduler.DeleteScheduleInput{
			GroupName: aws.String(group), Name: aws.String(s.Name),
		}); err != nil {
			return err
		}
	}
	return nil
}

// ensureScheduleGroup creates the group with cfg.Tags + tracking tag if it
// doesn't already exist. The "default" group always exists and is skipped.
// Existing groups are left untouched (we don't reconcile their tags) to avoid
// surprising side effects on groups shared with other tools.
func ensureScheduleGroup(ctx context.Context, cli *scheduler.Client, group string, cfg *Config, dryRun bool) error {
	if group == "default" {
		return nil
	}
	_, err := cli.GetScheduleGroup(ctx, &scheduler.GetScheduleGroupInput{
		Name: aws.String(group),
	})
	if err == nil {
		return nil
	}
	var nf *schtypes.ResourceNotFoundException
	if !errors.As(err, &nf) {
		return err
	}
	fmt.Printf("+ schedule-group:%s (create)\n", group)
	if dryRun {
		return nil
	}
	in := &scheduler.CreateScheduleGroupInput{Name: aws.String(group)}
	tags := map[string]string{}
	for k, v := range cfg.Tags {
		tags[k] = v
	}
	if cfg.TrackingID != "" {
		tags[trackingTagKey] = cfg.TrackingID
	}
	if len(tags) > 0 {
		awsTags := make([]schtypes.Tag, 0, len(tags))
		for k, v := range tags {
			awsTags = append(awsTags, schtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		in.Tags = awsTags
	}
	_, err = cli.CreateScheduleGroup(ctx, in)
	return err
}

func applyOneSchedule(ctx context.Context, cli *scheduler.Client, group string, cfg *Config, s *Schedule, dryRun bool) error {
	fmt.Printf("+ schedule:%s (apply)\n", s.Name)
	if dryRun {
		return nil
	}
	target, err := toAWSSchedTarget(s.Target)
	if err != nil {
		return err
	}
	state := schtypes.ScheduleStateEnabled
	if strings.EqualFold(s.State, "DISABLED") {
		state = schtypes.ScheduleStateDisabled
	}
	startDate, err := parseTime(s.StartDate)
	if err != nil {
		return err
	}
	endDate, err := parseTime(s.EndDate)
	if err != nil {
		return err
	}
	ftw := &schtypes.FlexibleTimeWindow{Mode: schtypes.FlexibleTimeWindowModeOff}
	if s.FlexibleTimeWindow != nil {
		ftw.Mode = schtypes.FlexibleTimeWindowMode(s.FlexibleTimeWindow.Mode)
		if s.FlexibleTimeWindow.MaximumWindowInMinutes > 0 {
			ftw.MaximumWindowInMinutes = aws.Int32(s.FlexibleTimeWindow.MaximumWindowInMinutes)
		}
	}

	exists := true
	if _, err := cli.GetSchedule(ctx, &scheduler.GetScheduleInput{
		GroupName: aws.String(group), Name: aws.String(s.Name),
	}); err != nil {
		var nf *schtypes.ResourceNotFoundException
		if !errors.As(err, &nf) {
			return err
		}
		exists = false
	}

	if exists {
		_, err = cli.UpdateSchedule(ctx, &scheduler.UpdateScheduleInput{
			Name:                       aws.String(s.Name),
			GroupName:                  aws.String(group),
			ScheduleExpression:         aws.String(s.ScheduleExpression),
			ScheduleExpressionTimezone: nilIfEmpty(s.ScheduleExpressionTimezone),
			State:                      state,
			Description:                nilIfEmpty(s.Description),
			KmsKeyArn:                  nilIfEmpty(s.KmsKeyArn),
			ActionAfterCompletion:      schtypes.ActionAfterCompletion(s.ActionAfterCompletion),
			FlexibleTimeWindow:         ftw,
			StartDate:                  startDate,
			EndDate:                    endDate,
			Target:                     target,
		})
	} else {
		_, err = cli.CreateSchedule(ctx, &scheduler.CreateScheduleInput{
			Name:                       aws.String(s.Name),
			GroupName:                  aws.String(group),
			ScheduleExpression:         aws.String(s.ScheduleExpression),
			ScheduleExpressionTimezone: nilIfEmpty(s.ScheduleExpressionTimezone),
			State:                      state,
			Description:                nilIfEmpty(s.Description),
			KmsKeyArn:                  nilIfEmpty(s.KmsKeyArn),
			ActionAfterCompletion:      schtypes.ActionAfterCompletion(s.ActionAfterCompletion),
			FlexibleTimeWindow:         ftw,
			StartDate:                  startDate,
			EndDate:                    endDate,
			Target:                     target,
		})
	}
	if err != nil {
		return err
	}

	// Reconcile tags (Create/Update do not accept Tags).
	desc, err := cli.GetSchedule(ctx, &scheduler.GetScheduleInput{
		GroupName: aws.String(group), Name: aws.String(s.Name),
	})
	if err != nil {
		return err
	}
	current, err := listSchedTags(ctx, cli, *desc.Arn)
	if err != nil {
		return err
	}
	desired := mergeTags(cfg.Tags, s.Tags)
	setFn := func(tags map[string]string) error {
		awsTags := make([]schtypes.Tag, 0, len(tags))
		for k, v := range tags {
			awsTags = append(awsTags, schtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		_, err := cli.TagResource(ctx, &scheduler.TagResourceInput{
			ResourceArn: desc.Arn, Tags: awsTags,
		})
		return err
	}
	unsetFn := func(keys []string) error {
		_, err := cli.UntagResource(ctx, &scheduler.UntagResourceInput{
			ResourceArn: desc.Arn, TagKeys: keys,
		})
		return err
	}
	return reconcileTags(current, desired, cfg.TrackingID, setFn, unsetFn)
}

func toAWSSchedTarget(t *ScheduleTarget) (*schtypes.Target, error) {
	if t == nil {
		return nil, fmt.Errorf("target is required")
	}
	at := &schtypes.Target{
		Arn:     aws.String(t.Arn),
		RoleArn: aws.String(t.RoleArn),
	}
	if t.Input != "" {
		at.Input = aws.String(t.Input)
	}
	if t.DeadLetterConfig != nil {
		at.DeadLetterConfig = &schtypes.DeadLetterConfig{Arn: aws.String(t.DeadLetterConfig.Arn)}
	}
	if t.RetryPolicy != nil {
		at.RetryPolicy = &schtypes.RetryPolicy{
			MaximumRetryAttempts:     aws.Int32(t.RetryPolicy.MaximumRetryAttempts),
			MaximumEventAgeInSeconds: aws.Int32(t.RetryPolicy.MaximumEventAgeInSeconds),
		}
	}
	if t.EcsParameters != nil {
		ep := &schtypes.EcsParameters{
			TaskDefinitionArn: aws.String(t.EcsParameters.TaskDefinitionArn),
			LaunchType:        schtypes.LaunchType(t.EcsParameters.LaunchType),
		}
		if t.EcsParameters.TaskCount > 0 {
			ep.TaskCount = aws.Int32(t.EcsParameters.TaskCount)
		}
		if t.EcsParameters.PlatformVersion != "" {
			ep.PlatformVersion = aws.String(t.EcsParameters.PlatformVersion)
		}
		if len(t.EcsParameters.Subnets) > 0 {
			ep.NetworkConfiguration = &schtypes.NetworkConfiguration{
				AwsvpcConfiguration: &schtypes.AwsVpcConfiguration{
					Subnets:        t.EcsParameters.Subnets,
					SecurityGroups: t.EcsParameters.SecurityGroups,
					AssignPublicIp: schtypes.AssignPublicIp(t.EcsParameters.AssignPublicIp),
				},
			}
		}
		at.EcsParameters = ep
	}
	if t.SqsParameters != nil {
		at.SqsParameters = &schtypes.SqsParameters{
			MessageGroupId: nilIfEmpty(t.SqsParameters.MessageGroupId),
		}
	}
	if t.EventBridgeParameters != nil {
		at.EventBridgeParameters = &schtypes.EventBridgeParameters{
			DetailType: aws.String(t.EventBridgeParameters.DetailType),
			Source:     aws.String(t.EventBridgeParameters.Source),
		}
	}
	return at, nil
}

func isSchedTracked(ctx context.Context, cli *scheduler.Client, group, name, trackingID string) (bool, error) {
	desc, err := cli.GetSchedule(ctx, &scheduler.GetScheduleInput{
		GroupName: aws.String(group), Name: aws.String(name),
	})
	if err != nil {
		return false, err
	}
	tags, err := listSchedTags(ctx, cli, *desc.Arn)
	if err != nil {
		return false, err
	}
	return tags[trackingTagKey] == trackingID, nil
}

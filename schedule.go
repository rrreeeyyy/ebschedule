package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schtypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
)

// schedAPI is the subset of *scheduler.Client that ebschedule actually uses.
// Defining it here lets tests inject a fake. *scheduler.Client satisfies it
// implicitly.
type schedAPI interface {
	ListSchedules(ctx context.Context, in *scheduler.ListSchedulesInput, optFns ...func(*scheduler.Options)) (*scheduler.ListSchedulesOutput, error)
	GetSchedule(ctx context.Context, in *scheduler.GetScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.GetScheduleOutput, error)
	ListScheduleGroups(ctx context.Context, in *scheduler.ListScheduleGroupsInput, optFns ...func(*scheduler.Options)) (*scheduler.ListScheduleGroupsOutput, error)
	GetScheduleGroup(ctx context.Context, in *scheduler.GetScheduleGroupInput, optFns ...func(*scheduler.Options)) (*scheduler.GetScheduleGroupOutput, error)
	CreateScheduleGroup(ctx context.Context, in *scheduler.CreateScheduleGroupInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleGroupOutput, error)
	CreateSchedule(ctx context.Context, in *scheduler.CreateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error)
	UpdateSchedule(ctx context.Context, in *scheduler.UpdateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.UpdateScheduleOutput, error)
	DeleteSchedule(ctx context.Context, in *scheduler.DeleteScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.DeleteScheduleOutput, error)
	ListTagsForResource(ctx context.Context, in *scheduler.ListTagsForResourceInput, optFns ...func(*scheduler.Options)) (*scheduler.ListTagsForResourceOutput, error)
	TagResource(ctx context.Context, in *scheduler.TagResourceInput, optFns ...func(*scheduler.Options)) (*scheduler.TagResourceOutput, error)
	UntagResource(ctx context.Context, in *scheduler.UntagResourceInput, optFns ...func(*scheduler.Options)) (*scheduler.UntagResourceOutput, error)
}

// --- types -----------------------------------------------------------------

// Schedule has no per-item Tags field: EventBridge Scheduler exposes tags
// only at the schedule-group level. Use Config.Tags (top-level) for tagging.
//
// GroupName, when non-empty, places this schedule in that group instead of
// the config-level cfg.GroupName. Lets one config manage schedules across
// multiple groups (e.g. shared cron jobs in one group, per-team schedules
// in another).
type Schedule struct {
	Name                       string              `yaml:"name"`
	Description                string              `yaml:"description,omitempty"`
	GroupName                  string              `yaml:"groupName,omitempty"`
	ScheduleExpression         string              `yaml:"scheduleExpression"`
	ScheduleExpressionTimezone string              `yaml:"scheduleExpressionTimezone,omitempty"`
	State                      string              `yaml:"state,omitempty"` // ENABLED | DISABLED
	StartDate                  string              `yaml:"startDate,omitempty"`
	EndDate                    string              `yaml:"endDate,omitempty"`
	KmsKeyArn                  string              `yaml:"kmsKeyArn,omitempty"`
	ActionAfterCompletion      string              `yaml:"actionAfterCompletion,omitempty"` // NONE | DELETE
	FlexibleTimeWindow         *FlexibleTimeWindow `yaml:"flexibleTimeWindow,omitempty"`
	Target                     *ScheduleTarget     `yaml:"target"`
}

// effectiveGroup returns the schedule's effective group: per-schedule
// override if set, otherwise the config's GroupName (or "default").
func (s *Schedule) effectiveGroup(cfg *Config) string {
	if s.GroupName != "" {
		return s.GroupName
	}
	return cfg.group()
}

type FlexibleTimeWindow struct {
	Mode                   string `yaml:"mode"` // OFF | FLEXIBLE
	MaximumWindowInMinutes int32  `yaml:"maximumWindowInMinutes,omitempty"`
}

type ScheduleTarget struct {
	Arn                         string                       `yaml:"arn"`
	RoleArn                     string                       `yaml:"roleArn"`
	Input                       jsonField                    `yaml:"input,omitempty"`
	DeadLetterConfig            *DeadLetterConfig            `yaml:"deadLetterConfig,omitempty"`
	RetryPolicy                 *RetryPolicy                 `yaml:"retryPolicy,omitempty"`
	EcsParameters               *SchedEcsParameters          `yaml:"ecsParameters,omitempty"`
	SqsParameters               *SqsParameters               `yaml:"sqsParameters,omitempty"`
	KinesisParameters           *SchedKinesisParameters      `yaml:"kinesisParameters,omitempty"`
	SageMakerPipelineParameters *SageMakerPipelineParameters `yaml:"sageMakerPipelineParameters,omitempty"`
	EventBridgeParameters       *EventBridgeParameters       `yaml:"eventBridgeParameters,omitempty"`
}

type SchedEcsParameters struct {
	TaskDefinitionArn        string                         `yaml:"taskDefinitionArn"`
	TaskCount                int32                          `yaml:"taskCount,omitempty"`
	LaunchType               string                         `yaml:"launchType,omitempty"`
	PlatformVersion          string                         `yaml:"platformVersion,omitempty"`
	Subnets                  []string                       `yaml:"subnets,omitempty"`
	SecurityGroups           []string                       `yaml:"securityGroups,omitempty"`
	AssignPublicIp           string                         `yaml:"assignPublicIp,omitempty"`
	Group                    string                         `yaml:"group,omitempty"`
	PropagateTags            string                         `yaml:"propagateTags,omitempty"` // TASK_DEFINITION
	CapacityProviderStrategy []CapacityProviderStrategyItem `yaml:"capacityProviderStrategy,omitempty"`
	EnableECSManagedTags     bool                           `yaml:"enableECSManagedTags,omitempty"`
	EnableExecuteCommand     bool                           `yaml:"enableExecuteCommand,omitempty"`
	PlacementConstraints     []PlacementConstraint          `yaml:"placementConstraints,omitempty"`
	PlacementStrategy        []PlacementStrategy            `yaml:"placementStrategy,omitempty"`
	ReferenceID              string                         `yaml:"referenceId,omitempty"`
	Tags                     []KeyValuePair                 `yaml:"tags,omitempty"` // ECS task tags
}

// SchedKinesisParameters is the Scheduler shape: a literal partition key
// (Rules use a JSON path instead).
type SchedKinesisParameters struct {
	PartitionKey string `yaml:"partitionKey"`
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
	return dumpSchedulesWith(ctx, cli, group, prefix)
}

func dumpSchedulesWith(ctx context.Context, cli schedAPI, group, prefix string) ([]*Schedule, error) {
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
			out = append(out, fromRemoteSchedule(full))
		}
		if aws.ToString(resp.NextToken) == "" {
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
		GroupName:                  aws.ToString(s.GroupName),
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
	return canonicalizeSchedule(out)
}

// canonicalizeSchedule returns a copy of s with fields stripped where they
// match Scheduler's documented defaults. AWS always returns these even when
// the user never set them, which would otherwise turn every diff into noise:
//
//   - timezone "UTC"                                       (default)
//   - actionAfterCompletion "NONE"                         (default)
//   - retryPolicy {MaximumRetryAttempts: 185,              (default)
//     MaximumEventAgeInSeconds: 86400}
//
// Called on both sides of `diff` so that explicit user-written defaults still
// match a stripped remote view. Non-destructive: the caller's *Schedule and
// nested *ScheduleTarget are never mutated.
func canonicalizeSchedule(s *Schedule) *Schedule {
	if s == nil {
		return nil
	}
	out := *s
	if out.ScheduleExpressionTimezone == "UTC" {
		out.ScheduleExpressionTimezone = ""
	}
	if out.ActionAfterCompletion == "NONE" {
		out.ActionAfterCompletion = ""
	}
	if t := out.Target; t != nil && t.RetryPolicy != nil &&
		t.RetryPolicy.MaximumRetryAttempts == schedDefaultMaximumRetryAttempts &&
		t.RetryPolicy.MaximumEventAgeInSeconds == schedDefaultMaximumEventAgeInSeconds {
		tgtCopy := *t
		tgtCopy.RetryPolicy = nil
		out.Target = &tgtCopy
	}
	return &out
}

func fromRemoteSchedTarget(t *schtypes.Target) *ScheduleTarget {
	st := &ScheduleTarget{
		Arn:     aws.ToString(t.Arn),
		RoleArn: aws.ToString(t.RoleArn),
		Input:   jsonFieldFromAWS(aws.ToString(t.Input)),
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
			TaskDefinitionArn:    aws.ToString(t.EcsParameters.TaskDefinitionArn),
			TaskCount:            aws.ToInt32(t.EcsParameters.TaskCount),
			LaunchType:           string(t.EcsParameters.LaunchType),
			PlatformVersion:      aws.ToString(t.EcsParameters.PlatformVersion),
			Group:                aws.ToString(t.EcsParameters.Group),
			PropagateTags:        string(t.EcsParameters.PropagateTags),
			EnableECSManagedTags: aws.ToBool(t.EcsParameters.EnableECSManagedTags),
			EnableExecuteCommand: aws.ToBool(t.EcsParameters.EnableExecuteCommand),
			ReferenceID:          aws.ToString(t.EcsParameters.ReferenceId),
		}
		if nc := t.EcsParameters.NetworkConfiguration; nc != nil && nc.AwsvpcConfiguration != nil {
			ep.Subnets = nc.AwsvpcConfiguration.Subnets
			ep.SecurityGroups = nc.AwsvpcConfiguration.SecurityGroups
			ep.AssignPublicIp = string(nc.AwsvpcConfiguration.AssignPublicIp)
		}
		for _, c := range t.EcsParameters.CapacityProviderStrategy {
			ep.CapacityProviderStrategy = append(ep.CapacityProviderStrategy, CapacityProviderStrategyItem{
				CapacityProvider: aws.ToString(c.CapacityProvider),
				Base:             c.Base,
				Weight:           c.Weight,
			})
		}
		for _, p := range t.EcsParameters.PlacementConstraints {
			ep.PlacementConstraints = append(ep.PlacementConstraints, PlacementConstraint{
				Type:       string(p.Type),
				Expression: aws.ToString(p.Expression),
			})
		}
		for _, p := range t.EcsParameters.PlacementStrategy {
			ep.PlacementStrategy = append(ep.PlacementStrategy, PlacementStrategy{
				Type:  string(p.Type),
				Field: aws.ToString(p.Field),
			})
		}
		// Scheduler returns ECS task tags as []map[string]string with a single
		// {key, value} pair per map.
		for _, m := range t.EcsParameters.Tags {
			ep.Tags = append(ep.Tags, KeyValuePair{Name: m["key"], Value: m["value"]})
		}
		st.EcsParameters = ep
	}
	if t.SqsParameters != nil {
		st.SqsParameters = &SqsParameters{MessageGroupId: aws.ToString(t.SqsParameters.MessageGroupId)}
	}
	if t.KinesisParameters != nil {
		st.KinesisParameters = &SchedKinesisParameters{
			PartitionKey: aws.ToString(t.KinesisParameters.PartitionKey),
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
		st.SageMakerPipelineParameters = smp
	}
	if t.EventBridgeParameters != nil {
		st.EventBridgeParameters = &EventBridgeParameters{
			DetailType: aws.ToString(t.EventBridgeParameters.DetailType),
			Source:     aws.ToString(t.EventBridgeParameters.Source),
		}
	}
	return st
}

// --- diff ------------------------------------------------------------------

// schedulesByGroup buckets cfg.Schedules by their effective group. Each
// schedule in the returned map has GroupName populated for canonical YAML
// comparison; the original *Schedule pointers are not mutated.
func schedulesByGroup(cfg *Config) map[string][]*Schedule {
	out := map[string][]*Schedule{}
	for _, s := range cfg.Schedules {
		g := s.effectiveGroup(cfg)
		copied := *s
		copied.GroupName = g
		out[g] = append(out[g], &copied)
	}
	return out
}

// sortedGroups returns map keys sorted for deterministic output.
func sortedGroups(byGroup map[string][]*Schedule) []string {
	groups := make([]string, 0, len(byGroup))
	for g := range byGroup {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return groups
}

// diffSchedules emits a unified diff per schedule to out and returns
// whether any schedule has drift. Schedules can specify their own group
// via Schedule.GroupName, so we dump per-group.
func diffSchedules(ctx context.Context, out io.Writer, cfg *Config) (bool, error) {
	byGroup := schedulesByGroup(cfg)
	drift := false
	for _, g := range sortedGroups(byGroup) {
		current, err := dumpSchedules(ctx, cfg.Region, g, "")
		if err != nil {
			return false, err
		}
		cur := map[string]*Schedule{}
		for _, s := range current {
			cur[s.Name] = s
		}
		for _, want := range byGroup[g] {
			// Canonicalize the user side too so an explicit
			// `scheduleExpressionTimezone: UTC` (or other defaulted value)
			// compares equal to a stripped remote view.
			desiredYAML := mustYAML(canonicalizeSchedule(want))
			got, ok := cur[want.Name]
			if !ok {
				fmt.Fprint(out, unifiedDiff("schedule:"+want.Name, "", desiredYAML))
				drift = true
				continue
			}
			gotYAML := mustYAML(got)
			if gotYAML == desiredYAML {
				continue
			}
			fmt.Fprint(out, unifiedDiff("schedule:"+want.Name, gotYAML, desiredYAML))
			drift = true
		}
	}
	return drift, nil
}

// --- apply -----------------------------------------------------------------

func applySchedules(ctx context.Context, out io.Writer, cfg *Config, dryRun, prune bool) error {
	cli, err := newSchedClient(ctx, cfg.Region)
	if err != nil {
		return err
	}
	return applySchedulesWith(ctx, out, cli, cfg, dryRun, prune)
}

func applySchedulesWith(ctx context.Context, out io.Writer, cli schedAPI, cfg *Config, dryRun, prune bool) error {
	byGroup := schedulesByGroup(cfg)

	for _, g := range sortedGroups(byGroup) {
		if err := ensureScheduleGroup(ctx, out, cli, g, cfg, dryRun); err != nil {
			return fmt.Errorf("ensure schedule group %s: %w", g, err)
		}
		for _, s := range byGroup[g] {
			if err := applyOneSchedule(ctx, out, cli, g, s, dryRun); err != nil {
				return fmt.Errorf("schedule %s: %w", s.Name, err)
			}
		}
	}

	if !prune {
		return nil
	}
	if cfg.TrackingID == "" {
		return fmt.Errorf("-prune requires trackingId in config (safety guard)")
	}

	// Prune scans every schedule-group in the account that carries our
	// tracking-id, not just groups currently referenced by cfg.Schedules.
	// Otherwise removing a schedule (and its now-unreferenced group) from
	// the config would silently leave the orphan in AWS.
	tracked, err := listTrackedGroups(ctx, cli, cfg.TrackingID)
	if err != nil {
		return err
	}
	for _, g := range tracked {
		current, err := dumpSchedulesWith(ctx, cli, g, "")
		if err != nil {
			return err
		}
		desiredInGroup := map[string]bool{}
		for _, s := range byGroup[g] {
			desiredInGroup[s.Name] = true
		}
		for _, s := range current {
			if desiredInGroup[s.Name] {
				continue
			}
			fmt.Fprintf(out, "- schedule:%s (delete from %s)\n", s.Name, g)
			if dryRun {
				continue
			}
			if _, err := cli.DeleteSchedule(ctx, &scheduler.DeleteScheduleInput{
				GroupName: aws.String(g), Name: aws.String(s.Name),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// listTrackedGroups returns the names of every schedule-group whose tags
// contain ebschedule-tracking-id=<trackingID>. Sorted for determinism.
//
// We use the ARN from ListScheduleGroups directly rather than calling
// GetScheduleGroup, because GetScheduleGroup returns a nil Arn for the
// special "default" group. ListScheduleGroups consistently returns ARNs
// for all groups including "default".
func listTrackedGroups(ctx context.Context, cli schedAPI, trackingID string) ([]string, error) {
	var token *string
	var out []string
	for {
		resp, err := cli.ListScheduleGroups(ctx, &scheduler.ListScheduleGroupsInput{NextToken: token})
		if err != nil {
			return nil, err
		}
		for _, g := range resp.ScheduleGroups {
			arn := aws.ToString(g.Arn)
			if arn == "" {
				continue
			}
			tagResp, err := cli.ListTagsForResource(ctx, &scheduler.ListTagsForResourceInput{
				ResourceArn: aws.String(arn),
			})
			if err != nil {
				return nil, err
			}
			for _, t := range tagResp.Tags {
				if aws.ToString(t.Key) == trackingTagKey && aws.ToString(t.Value) == trackingID {
					out = append(out, aws.ToString(g.Name))
					break
				}
			}
		}
		if aws.ToString(resp.NextToken) == "" {
			break
		}
		token = resp.NextToken
	}
	sort.Strings(out)
	return out, nil
}

// ensureScheduleGroup creates the group with cfg.Tags + tracking tag if it
// doesn't already exist. The "default" group always exists and is skipped.
// Existing groups are left untouched (we don't reconcile their tags) to avoid
// surprising side effects on groups shared with other tools.
func ensureScheduleGroup(ctx context.Context, out io.Writer, cli schedAPI, group string, cfg *Config, dryRun bool) error {
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
	fmt.Fprintf(out, "+ schedule-group:%s (create)\n", group)
	if dryRun {
		return nil
	}
	in := &scheduler.CreateScheduleGroupInput{Name: aws.String(group)}
	tags := map[string]string{}
	maps.Copy(tags, cfg.Tags)
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

func applyOneSchedule(ctx context.Context, out io.Writer, cli schedAPI, group string, s *Schedule, dryRun bool) error {
	got, err := cli.GetSchedule(ctx, &scheduler.GetScheduleInput{
		GroupName: aws.String(group), Name: aws.String(s.Name),
	})
	exists := true
	if err != nil {
		var nf *schtypes.ResourceNotFoundException
		if !errors.As(err, &nf) {
			return err
		}
		exists = false
	}

	desiredYAML := mustYAML(canonicalizeSchedule(s))

	switch {
	case !exists:
		fmt.Fprintf(out, "+ schedule:%s (create)\n", s.Name)
	default:
		current := fromRemoteSchedule(got)
		if mustYAML(current) == desiredYAML {
			fmt.Fprintf(out, "= schedule:%s (no-op)\n", s.Name)
			return nil
		}
		fmt.Fprintf(out, "~ schedule:%s (update)\n", s.Name)
	}
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
	return err
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
		at.Input = aws.String(string(t.Input))
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
		if t.EcsParameters.Group != "" {
			ep.Group = aws.String(t.EcsParameters.Group)
		}
		if t.EcsParameters.PropagateTags != "" {
			ep.PropagateTags = schtypes.PropagateTags(t.EcsParameters.PropagateTags)
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
		for _, c := range t.EcsParameters.CapacityProviderStrategy {
			ep.CapacityProviderStrategy = append(ep.CapacityProviderStrategy, schtypes.CapacityProviderStrategyItem{
				CapacityProvider: aws.String(c.CapacityProvider),
				Base:             c.Base,
				Weight:           c.Weight,
			})
		}
		if t.EcsParameters.EnableECSManagedTags {
			ep.EnableECSManagedTags = aws.Bool(true)
		}
		if t.EcsParameters.EnableExecuteCommand {
			ep.EnableExecuteCommand = aws.Bool(true)
		}
		for _, p := range t.EcsParameters.PlacementConstraints {
			ep.PlacementConstraints = append(ep.PlacementConstraints, schtypes.PlacementConstraint{
				Type:       schtypes.PlacementConstraintType(p.Type),
				Expression: nilIfEmpty(p.Expression),
			})
		}
		for _, p := range t.EcsParameters.PlacementStrategy {
			ep.PlacementStrategy = append(ep.PlacementStrategy, schtypes.PlacementStrategy{
				Type:  schtypes.PlacementStrategyType(p.Type),
				Field: nilIfEmpty(p.Field),
			})
		}
		if t.EcsParameters.ReferenceID != "" {
			ep.ReferenceId = aws.String(t.EcsParameters.ReferenceID)
		}
		// Scheduler ECS task tags shape: []map[string]string with one
		// {key, value} pair per element.
		for _, kv := range t.EcsParameters.Tags {
			ep.Tags = append(ep.Tags, map[string]string{"key": kv.Name, "value": kv.Value})
		}
		at.EcsParameters = ep
	}
	if t.SqsParameters != nil {
		at.SqsParameters = &schtypes.SqsParameters{
			MessageGroupId: nilIfEmpty(t.SqsParameters.MessageGroupId),
		}
	}
	if t.KinesisParameters != nil {
		at.KinesisParameters = &schtypes.KinesisParameters{
			PartitionKey: aws.String(t.KinesisParameters.PartitionKey),
		}
	}
	if t.SageMakerPipelineParameters != nil {
		smp := &schtypes.SageMakerPipelineParameters{}
		for _, p := range t.SageMakerPipelineParameters.PipelineParameterList {
			smp.PipelineParameterList = append(smp.PipelineParameterList, schtypes.SageMakerPipelineParameter{
				Name:  aws.String(p.Name),
				Value: aws.String(p.Value),
			})
		}
		at.SageMakerPipelineParameters = smp
	}
	if t.EventBridgeParameters != nil {
		at.EventBridgeParameters = &schtypes.EventBridgeParameters{
			DetailType: aws.String(t.EventBridgeParameters.DetailType),
			Source:     aws.String(t.EventBridgeParameters.Source),
		}
	}
	return at, nil
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/winebarrel/cronplan"
)

// Resource name regex used by both EventBridge Rules and Scheduler Schedules.
// AWS docs differ slightly on whether Scheduler permits underscores; testing
// the API today shows it does, so we keep a single regex.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)

// Schedule expressions accepted by Scheduler / EventBridge. Used as a coarse
// shape check before validateCronExpression does the real parse for cron().
var schedExprRe = regexp.MustCompile(`^(cron|rate|at)\(.+\)\s*$`)

// validateScheduleExpression checks the wrapper shape and, for cron(...),
// runs cronplan against the inner expression so typos like an out-of-range
// minute or a missing day-of-week field fail at load time rather than at
// AWS-call time. rate(...) and at(...) get the regex-only check for now —
// AWS rejects malformed ones with a clear API error, and the rate / at
// grammar is small enough that the regex catches the common typos.
func validateScheduleExpression(expr string) error {
	if expr == "" {
		return fmt.Errorf("scheduleExpression is empty")
	}
	if !schedExprRe.MatchString(expr) {
		return fmt.Errorf("scheduleExpression %q must look like cron(...), rate(...) or at(...)", expr)
	}
	if !strings.HasPrefix(expr, "cron(") {
		return nil
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(expr, "cron("), ")")
	if inner != strings.TrimSpace(inner) {
		return fmt.Errorf("scheduleExpression %q has stray whitespace inside cron(...)", expr)
	}
	if _, err := cronplan.Parse(inner); err != nil {
		return fmt.Errorf("scheduleExpression %q: %w", expr, err)
	}
	return nil
}

// Documented Scheduler defaults; canonicalizeSchedule drops user-side values
// that match these so diff stays whitespace-and-default-insensitive.
const (
	schedDefaultMaximumRetryAttempts     = 185
	schedDefaultMaximumEventAgeInSeconds = 86400
)

// runValidate runs offline validation across all loaded configs and exits
// non-zero if any errors were found. It prints every problem rather than
// stopping at the first.
func runValidate(cfgs []*Config) error {
	total := 0
	for _, c := range cfgs {
		errs := validateConfig(c)
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "%s: %s\n", c.sourcePath, e)
		}
		total += len(errs)
	}
	if total > 0 {
		return fmt.Errorf("validation failed: %d problem(s)", total)
	}
	fmt.Printf("OK: %d config file(s) valid\n", len(cfgs))
	return nil
}

func validateConfig(c *Config) []string {
	var errs []string

	// Top-level tags.
	for k, v := range c.Tags {
		errs = append(errs, validateTag(k, v, "tags")...)
	}

	// Rules.
	ruleNames := map[string]int{}
	for i, r := range c.Rules {
		path := fmt.Sprintf("rule[%d]:%s", i, r.Name)
		if r.Name == "" {
			errs = append(errs, fmt.Sprintf("%s: name is required", path))
		} else if !nameRe.MatchString(r.Name) || len(r.Name) > 64 {
			errs = append(errs, fmt.Sprintf("%s: name must match %s and be <=64 chars", path, nameRe))
		}
		if prev, dup := ruleNames[r.Name]; dup && r.Name != "" {
			errs = append(errs, fmt.Sprintf("%s: duplicate name (also at rule[%d])", path, prev))
		}
		ruleNames[r.Name] = i
		errs = append(errs, validateRule(r, path)...)
	}

	// Schedules.
	schedNames := map[string]int{}
	for i, s := range c.Schedules {
		path := fmt.Sprintf("schedule[%d]:%s", i, s.Name)
		if s.Name == "" {
			errs = append(errs, fmt.Sprintf("%s: name is required", path))
		} else if !nameRe.MatchString(s.Name) || len(s.Name) > 64 {
			errs = append(errs, fmt.Sprintf("%s: name must match %s and be <=64 chars", path, nameRe))
		}
		if prev, dup := schedNames[s.Name]; dup && s.Name != "" {
			errs = append(errs, fmt.Sprintf("%s: duplicate name (also at schedule[%d])", path, prev))
		}
		schedNames[s.Name] = i
		errs = append(errs, validateSchedule(s, path)...)
	}

	if c.Rules == nil && c.Schedules == nil {
		errs = append(errs, "neither rules: nor schedules: present")
	}
	return errs
}

func validateRule(r *Rule, path string) []string {
	var errs []string

	if r.ScheduleExpression == "" && r.EventPattern == "" {
		errs = append(errs, fmt.Sprintf("%s: must set either scheduleExpression or eventPattern", path))
	}
	if r.ScheduleExpression != "" && r.EventPattern != "" {
		errs = append(errs, fmt.Sprintf("%s: scheduleExpression and eventPattern are mutually exclusive", path))
	}
	if r.ScheduleExpression != "" {
		if err := validateScheduleExpression(r.ScheduleExpression); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", path, err))
		}
	}
	if r.EventPattern != "" {
		if !json.Valid([]byte(r.EventPattern)) {
			errs = append(errs, fmt.Sprintf("%s: eventPattern is not valid JSON", path))
		}
	}
	if r.State != "" && r.State != "ENABLED" && r.State != "DISABLED" {
		errs = append(errs, fmt.Sprintf("%s: state must be ENABLED or DISABLED", path))
	}
	for k, v := range r.Tags {
		errs = append(errs, validateTag(k, v, path+".tags")...)
	}

	if len(r.Targets) == 0 {
		errs = append(errs, fmt.Sprintf("%s: at least one target is required", path))
	}
	if len(r.Targets) > 5 {
		errs = append(errs, fmt.Sprintf("%s: a rule can have at most 5 targets (got %d)", path, len(r.Targets)))
	}
	tids := map[string]int{}
	for j, t := range r.Targets {
		tp := fmt.Sprintf("%s.targets[%d]:%s", path, j, t.ID)
		if t.ID == "" {
			errs = append(errs, fmt.Sprintf("%s: target id is required", tp))
		} else if prev, dup := tids[t.ID]; dup {
			errs = append(errs, fmt.Sprintf("%s: duplicate id (also at targets[%d])", tp, prev))
		}
		tids[t.ID] = j
		if t.Arn == "" {
			errs = append(errs, fmt.Sprintf("%s: arn is required", tp))
		} else if !looksLikeArnOrPlaceholder(t.Arn) {
			errs = append(errs, fmt.Sprintf("%s: arn does not look like an ARN", tp))
		}
		if t.Input != "" && !json.Valid([]byte(t.Input)) {
			errs = append(errs, fmt.Sprintf("%s.input: not valid JSON", tp))
		}
		if t.InputTransformer != nil && t.InputTransformer.InputTemplate == "" {
			errs = append(errs, fmt.Sprintf("%s.inputTransformer.inputTemplate: is required", tp))
		}
		// EventBridge accepts at most one of Input / InputPath / InputTransformer
		// per target. AWS silently picks one if multiple are set, which is
		// surprising; reject up front.
		inputModes := 0
		if t.Input != "" {
			inputModes++
		}
		if t.InputPath != "" {
			inputModes++
		}
		if t.InputTransformer != nil {
			inputModes++
		}
		if inputModes > 1 {
			errs = append(errs, fmt.Sprintf("%s: input, inputPath, and inputTransformer are mutually exclusive", tp))
		}
		if t.EcsParameters != nil {
			errs = append(errs, validateEcsCommon(t.EcsParameters.TaskDefinitionArn, t.EcsParameters.LaunchType, t.EcsParameters.AssignPublicIp,
				t.EcsParameters.CapacityProviderStrategy, t.EcsParameters.PlacementConstraints, t.EcsParameters.PlacementStrategy,
				tp+".ecsParameters")...)
		}
		if t.KinesisParameters != nil && t.KinesisParameters.PartitionKeyPath == "" {
			errs = append(errs, fmt.Sprintf("%s.kinesisParameters.partitionKeyPath: is required", tp))
		}
		if t.BatchParameters != nil {
			if t.BatchParameters.JobDefinition == "" {
				errs = append(errs, fmt.Sprintf("%s.batchParameters.jobDefinition: is required", tp))
			}
			if t.BatchParameters.JobName == "" {
				errs = append(errs, fmt.Sprintf("%s.batchParameters.jobName: is required", tp))
			}
		}
		if t.RedshiftDataParameters != nil {
			rp := t.RedshiftDataParameters
			if rp.Database == "" {
				errs = append(errs, fmt.Sprintf("%s.redshiftDataParameters.database: is required", tp))
			}
			if rp.Sql == "" && len(rp.Sqls) == 0 {
				errs = append(errs, fmt.Sprintf("%s.redshiftDataParameters: must set either sql or sqls", tp))
			}
			if rp.Sql != "" && len(rp.Sqls) > 0 {
				errs = append(errs, fmt.Sprintf("%s.redshiftDataParameters: sql and sqls are mutually exclusive", tp))
			}
		}
		if t.SageMakerPipelineParameters != nil {
			for j, p := range t.SageMakerPipelineParameters.PipelineParameterList {
				if p.Name == "" {
					errs = append(errs, fmt.Sprintf("%s.sageMakerPipelineParameters.pipelineParameterList[%d].name: is required", tp, j))
				}
			}
		}
	}
	return errs
}

func validateSchedule(s *Schedule, path string) []string {
	var errs []string

	if s.ScheduleExpression == "" {
		errs = append(errs, fmt.Sprintf("%s: scheduleExpression is required", path))
	} else if err := validateScheduleExpression(s.ScheduleExpression); err != nil {
		errs = append(errs, fmt.Sprintf("%s: %s", path, err))
	}
	if s.ScheduleExpressionTimezone != "" {
		if _, err := time.LoadLocation(s.ScheduleExpressionTimezone); err != nil {
			errs = append(errs, fmt.Sprintf("%s.timezone: invalid IANA timezone %q", path, s.ScheduleExpressionTimezone))
		}
	}
	if s.State != "" && s.State != "ENABLED" && s.State != "DISABLED" {
		errs = append(errs, fmt.Sprintf("%s.state: must be ENABLED or DISABLED", path))
	}
	if a := s.ActionAfterCompletion; a != "" && a != "NONE" && a != "DELETE" {
		errs = append(errs, fmt.Sprintf("%s.actionAfterCompletion: must be NONE or DELETE", path))
	}
	var startTime, endTime time.Time
	for _, pair := range []struct {
		name string
		val  string
		out  *time.Time
	}{{"startDate", s.StartDate, &startTime}, {"endDate", s.EndDate, &endTime}} {
		if pair.val == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, pair.val)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s.%s: must be RFC3339", path, pair.name))
			continue
		}
		*pair.out = t
	}
	if !startTime.IsZero() && !endTime.IsZero() && !endTime.After(startTime) {
		errs = append(errs, fmt.Sprintf("%s: endDate must be after startDate", path))
	}
	if ftw := s.FlexibleTimeWindow; ftw != nil {
		if ftw.Mode != "OFF" && ftw.Mode != "FLEXIBLE" {
			errs = append(errs, fmt.Sprintf("%s.flexibleTimeWindow.mode: must be OFF or FLEXIBLE", path))
		}
		if ftw.Mode == "FLEXIBLE" && ftw.MaximumWindowInMinutes <= 0 {
			errs = append(errs, fmt.Sprintf("%s.flexibleTimeWindow.maximumWindowInMinutes: must be > 0 when mode=FLEXIBLE", path))
		}
	}
	if s.Target == nil {
		errs = append(errs, fmt.Sprintf("%s.target: is required", path))
		return errs
	}
	tp := path + ".target"
	if s.Target.Arn == "" {
		errs = append(errs, fmt.Sprintf("%s.arn: is required", tp))
	} else if !looksLikeArnOrPlaceholder(s.Target.Arn) {
		errs = append(errs, fmt.Sprintf("%s.arn: does not look like an ARN", tp))
	}
	if s.Target.RoleArn == "" {
		errs = append(errs, fmt.Sprintf("%s.roleArn: is required", tp))
	}
	if s.Target.Input != "" && !json.Valid([]byte(s.Target.Input)) {
		errs = append(errs, fmt.Sprintf("%s.input: not valid JSON", tp))
	}
	if s.Target.EcsParameters != nil {
		errs = append(errs, validateEcsCommon(s.Target.EcsParameters.TaskDefinitionArn, s.Target.EcsParameters.LaunchType,
			s.Target.EcsParameters.AssignPublicIp, s.Target.EcsParameters.CapacityProviderStrategy,
			s.Target.EcsParameters.PlacementConstraints, s.Target.EcsParameters.PlacementStrategy,
			tp+".ecsParameters")...)
	}
	if s.Target.KinesisParameters != nil && s.Target.KinesisParameters.PartitionKey == "" {
		errs = append(errs, fmt.Sprintf("%s.kinesisParameters.partitionKey: is required", tp))
	}
	if s.Target.SageMakerPipelineParameters != nil {
		for j, p := range s.Target.SageMakerPipelineParameters.PipelineParameterList {
			if p.Name == "" {
				errs = append(errs, fmt.Sprintf("%s.sageMakerPipelineParameters.pipelineParameterList[%d].name: is required", tp, j))
			}
		}
	}
	return errs
}

// looksLikeArnOrPlaceholder accepts both real ARNs (`arn:...`) and the
// `<tfstate:...>` / `<ssm:...>` placeholders that validate's offline
// FuncMap emits, so a config that pulls ARNs from tfstate at apply time
// still passes structural validation when run via `validate`.
func looksLikeArnOrPlaceholder(s string) bool {
	return strings.HasPrefix(s, "arn:") || strings.HasPrefix(s, "<")
}

// validateEcsCommon covers the fields shared between RuleEcsParameters
// and SchedEcsParameters. Both have the same enum constraints and the
// same launchType / capacityProviderStrategy mutual-exclusion rule.
func validateEcsCommon(
	taskDefinitionArn, launchType, assignPublicIp string,
	cps []CapacityProviderStrategyItem,
	placementConstraints []PlacementConstraint,
	placementStrategy []PlacementStrategy,
	path string,
) []string {
	var errs []string
	if taskDefinitionArn == "" {
		errs = append(errs, fmt.Sprintf("%s.taskDefinitionArn: is required", path))
	}
	if launchType != "" && launchType != "EC2" && launchType != "FARGATE" && launchType != "EXTERNAL" {
		errs = append(errs, fmt.Sprintf("%s.launchType: must be EC2/FARGATE/EXTERNAL", path))
	}
	if assignPublicIp != "" && assignPublicIp != "ENABLED" && assignPublicIp != "DISABLED" {
		errs = append(errs, fmt.Sprintf("%s.assignPublicIp: must be ENABLED or DISABLED", path))
	}
	if launchType != "" && len(cps) > 0 {
		errs = append(errs, fmt.Sprintf("%s: launchType and capacityProviderStrategy are mutually exclusive", path))
	}
	for j, c := range cps {
		if c.CapacityProvider == "" {
			errs = append(errs, fmt.Sprintf("%s.capacityProviderStrategy[%d].capacityProvider: is required", path, j))
		}
	}
	for j, p := range placementConstraints {
		if p.Type != "distinctInstance" && p.Type != "memberOf" {
			errs = append(errs, fmt.Sprintf("%s.placementConstraints[%d].type: must be distinctInstance or memberOf", path, j))
		}
		if p.Type == "memberOf" && p.Expression == "" {
			errs = append(errs, fmt.Sprintf("%s.placementConstraints[%d].expression: is required when type=memberOf", path, j))
		}
	}
	for j, p := range placementStrategy {
		if p.Type != "random" && p.Type != "spread" && p.Type != "binpack" {
			errs = append(errs, fmt.Sprintf("%s.placementStrategy[%d].type: must be random/spread/binpack", path, j))
		}
	}
	return errs
}

func validateTag(k, v, path string) []string {
	var errs []string
	if len(k) < 1 || len(k) > 128 {
		errs = append(errs, fmt.Sprintf("%s: tag key %q must be 1-128 chars", path, k))
	}
	if strings.HasPrefix(strings.ToLower(k), "aws:") {
		errs = append(errs, fmt.Sprintf("%s: tag key %q cannot start with aws:", path, k))
	}
	if len(v) > 256 {
		errs = append(errs, fmt.Sprintf("%s: tag value for %q must be <=256 chars", path, k))
	}
	if k == trackingTagKey {
		errs = append(errs, fmt.Sprintf("%s: tag key %q is reserved by ebschedule", path, k))
	}
	return errs
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Resource name regex used by both EventBridge Rules and Scheduler Schedules.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)

// Schedule expressions accepted by Scheduler / EventBridge.
var schedExprRe = regexp.MustCompile(`^(cron|rate|at)\(.+\)\s*$`)

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
	if r.ScheduleExpression != "" && !schedExprRe.MatchString(r.ScheduleExpression) {
		errs = append(errs, fmt.Sprintf("%s: scheduleExpression must look like cron(...) or rate(...)", path))
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
		} else if !strings.HasPrefix(t.Arn, "arn:") {
			errs = append(errs, fmt.Sprintf("%s: arn does not look like an ARN", tp))
		}
		if t.Input != "" && !json.Valid([]byte(t.Input)) {
			errs = append(errs, fmt.Sprintf("%s.input: not valid JSON", tp))
		}
		if t.InputTransformer != nil && t.InputTransformer.InputTemplate == "" {
			errs = append(errs, fmt.Sprintf("%s.inputTransformer.inputTemplate: is required", tp))
		}
		if t.EcsParameters != nil {
			if t.EcsParameters.TaskDefinitionArn == "" {
				errs = append(errs, fmt.Sprintf("%s.ecsParameters.taskDefinitionArn: is required", tp))
			}
			if lt := t.EcsParameters.LaunchType; lt != "" && lt != "EC2" && lt != "FARGATE" && lt != "EXTERNAL" {
				errs = append(errs, fmt.Sprintf("%s.ecsParameters.launchType: must be EC2/FARGATE/EXTERNAL", tp))
			}
			if ap := t.EcsParameters.AssignPublicIp; ap != "" && ap != "ENABLED" && ap != "DISABLED" {
				errs = append(errs, fmt.Sprintf("%s.ecsParameters.assignPublicIp: must be ENABLED or DISABLED", tp))
			}
		}
	}
	return errs
}

func validateSchedule(s *Schedule, path string) []string {
	var errs []string

	if s.ScheduleExpression == "" {
		errs = append(errs, fmt.Sprintf("%s: scheduleExpression is required", path))
	} else if !schedExprRe.MatchString(s.ScheduleExpression) {
		errs = append(errs, fmt.Sprintf("%s: scheduleExpression must look like cron(...), rate(...) or at(...)", path))
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
	for _, pair := range []struct{ name, val string }{{"startDate", s.StartDate}, {"endDate", s.EndDate}} {
		if pair.val == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, pair.val); err != nil {
			errs = append(errs, fmt.Sprintf("%s.%s: must be RFC3339", path, pair.name))
		}
	}
	if ftw := s.FlexibleTimeWindow; ftw != nil {
		if ftw.Mode != "OFF" && ftw.Mode != "FLEXIBLE" {
			errs = append(errs, fmt.Sprintf("%s.flexibleTimeWindow.mode: must be OFF or FLEXIBLE", path))
		}
		if ftw.Mode == "FLEXIBLE" && ftw.MaximumWindowInMinutes <= 0 {
			errs = append(errs, fmt.Sprintf("%s.flexibleTimeWindow.maximumWindowInMinutes: must be > 0 when mode=FLEXIBLE", path))
		}
	}
	for k, v := range s.Tags {
		errs = append(errs, validateTag(k, v, path+".tags")...)
	}

	if s.Target == nil {
		errs = append(errs, fmt.Sprintf("%s.target: is required", path))
		return errs
	}
	tp := path + ".target"
	if s.Target.Arn == "" {
		errs = append(errs, fmt.Sprintf("%s.arn: is required", tp))
	} else if !strings.HasPrefix(s.Target.Arn, "arn:") {
		errs = append(errs, fmt.Sprintf("%s.arn: does not look like an ARN", tp))
	}
	if s.Target.RoleArn == "" {
		errs = append(errs, fmt.Sprintf("%s.roleArn: is required", tp))
	}
	if s.Target.Input != "" && !json.Valid([]byte(s.Target.Input)) {
		errs = append(errs, fmt.Sprintf("%s.input: not valid JSON", tp))
	}
	if s.Target.EcsParameters != nil {
		if s.Target.EcsParameters.TaskDefinitionArn == "" {
			errs = append(errs, fmt.Sprintf("%s.ecsParameters.taskDefinitionArn: is required", tp))
		}
		if lt := s.Target.EcsParameters.LaunchType; lt != "" && lt != "EC2" && lt != "FARGATE" && lt != "EXTERNAL" {
			errs = append(errs, fmt.Sprintf("%s.ecsParameters.launchType: must be EC2/FARGATE/EXTERNAL", tp))
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

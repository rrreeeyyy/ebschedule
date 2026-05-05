package main

import (
	"strings"
	"testing"
)

func validRule() *Rule {
	return &Rule{
		Name:               "ok-rule",
		ScheduleExpression: "cron(0 9 * * ? *)",
		Targets: []*Target{
			{ID: "t1", Arn: "arn:aws:lambda:us-east-1:123456789012:function:f"},
		},
	}
}

func validSchedule() *Schedule {
	return &Schedule{
		Name:               "ok-sched",
		ScheduleExpression: "rate(5 minutes)",
		FlexibleTimeWindow: &FlexibleTimeWindow{Mode: "OFF"},
		Target: &ScheduleTarget{
			Arn:     "arn:aws:lambda:us-east-1:123456789012:function:f",
			RoleArn: "arn:aws:iam::123456789012:role/r",
		},
	}
}

func wantSubstr(t *testing.T, errs []string, sub string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e, sub) {
			return
		}
	}
	t.Fatalf("no error contained %q; got %v", sub, errs)
}

func TestValidateConfig_BothEmpty(t *testing.T) {
	errs := validateConfig(&Config{})
	wantSubstr(t, errs, "neither rules: nor schedules: present")
}

func TestValidateConfig_HappyPath(t *testing.T) {
	c := &Config{
		Rules:     []*Rule{validRule()},
		Schedules: []*Schedule{validSchedule()},
		Tags:      map[string]string{"env": "prod"},
	}
	if errs := validateConfig(c); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateConfig_DuplicateNames(t *testing.T) {
	r1, r2 := validRule(), validRule()
	s1, s2 := validSchedule(), validSchedule()
	c := &Config{Rules: []*Rule{r1, r2}, Schedules: []*Schedule{s1, s2}}
	errs := validateConfig(c)
	wantSubstr(t, errs, "rule[1]:ok-rule: duplicate name")
	wantSubstr(t, errs, "schedule[1]:ok-sched: duplicate name")
}

func TestValidateRule_NameRules(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Rule)
		want string
	}{
		{"empty name", func(r *Rule) { r.Name = "" }, "name is required"},
		{"bad chars", func(r *Rule) { r.Name = "bad name!" }, "name must match"},
		{"too long", func(r *Rule) { r.Name = strings.Repeat("a", 65) }, "<=64 chars"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRule()
			tc.set(r)
			c := &Config{Rules: []*Rule{r}}
			wantSubstr(t, validateConfig(c), tc.want)
		})
	}
}

func TestValidateRule_Expression(t *testing.T) {
	t.Run("neither set", func(t *testing.T) {
		r := validRule()
		r.ScheduleExpression = ""
		errs := validateRule(r, "rule[0]:x")
		wantSubstr(t, errs, "must set either scheduleExpression or eventPattern")
	})
	t.Run("both set", func(t *testing.T) {
		r := validRule()
		r.EventPattern = `{"source":["x"]}`
		wantSubstr(t, validateRule(r, "rule[0]:x"), "mutually exclusive")
	})
	t.Run("malformed schedule expression", func(t *testing.T) {
		r := validRule()
		r.ScheduleExpression = "every 5 minutes"
		wantSubstr(t, validateRule(r, "rule[0]:x"), "scheduleExpression must look like")
	})
	t.Run("invalid event pattern JSON", func(t *testing.T) {
		r := validRule()
		r.ScheduleExpression = ""
		r.EventPattern = "{not json"
		wantSubstr(t, validateRule(r, "rule[0]:x"), "eventPattern is not valid JSON")
	})
	t.Run("invalid state", func(t *testing.T) {
		r := validRule()
		r.State = "PAUSED"
		wantSubstr(t, validateRule(r, "rule[0]:x"), "state must be ENABLED or DISABLED")
	})
}

func TestValidateRule_Targets(t *testing.T) {
	t.Run("zero targets", func(t *testing.T) {
		r := validRule()
		r.Targets = nil
		wantSubstr(t, validateRule(r, "rule[0]:x"), "at least one target is required")
	})
	t.Run("too many targets", func(t *testing.T) {
		r := validRule()
		for range 5 {
			r.Targets = append(r.Targets, &Target{
				ID:  "extra",
				Arn: "arn:aws:lambda:us-east-1:123456789012:function:f",
			})
		}
		wantSubstr(t, validateRule(r, "rule[0]:x"), "at most 5 targets")
	})
	t.Run("missing id and arn", func(t *testing.T) {
		r := validRule()
		r.Targets = []*Target{{}}
		errs := validateRule(r, "rule[0]:x")
		wantSubstr(t, errs, "target id is required")
		wantSubstr(t, errs, "arn is required")
	})
	t.Run("duplicate id", func(t *testing.T) {
		r := validRule()
		r.Targets = []*Target{
			{ID: "same", Arn: "arn:a"},
			{ID: "same", Arn: "arn:b"},
		}
		wantSubstr(t, validateRule(r, "rule[0]:x"), "duplicate id")
	})
	t.Run("non-arn", func(t *testing.T) {
		r := validRule()
		r.Targets[0].Arn = "lambda:f"
		wantSubstr(t, validateRule(r, "rule[0]:x"), "does not look like an ARN")
	})
	t.Run("invalid input JSON", func(t *testing.T) {
		r := validRule()
		r.Targets[0].Input = "{nope"
		wantSubstr(t, validateRule(r, "rule[0]:x"), "input: not valid JSON")
	})
	t.Run("inputTransformer missing template", func(t *testing.T) {
		r := validRule()
		r.Targets[0].InputTransformer = &InputTransformer{}
		wantSubstr(t, validateRule(r, "rule[0]:x"), "inputTransformer.inputTemplate")
	})
	t.Run("ecs missing taskDef", func(t *testing.T) {
		r := validRule()
		r.Targets[0].EcsParameters = &RuleEcsParameters{}
		wantSubstr(t, validateRule(r, "rule[0]:x"), "ecsParameters.taskDefinitionArn")
	})
	t.Run("ecs invalid launchType / assignPublicIp", func(t *testing.T) {
		r := validRule()
		r.Targets[0].EcsParameters = &RuleEcsParameters{
			TaskDefinitionArn: "arn:aws:ecs:us-east-1:1:task-definition/x:1",
			LaunchType:        "SERVERLESS",
			AssignPublicIp:    "MAYBE",
		}
		errs := validateRule(r, "rule[0]:x")
		wantSubstr(t, errs, "ecsParameters.launchType")
		wantSubstr(t, errs, "ecsParameters.assignPublicIp")
	})
}

func TestValidateSchedule(t *testing.T) {
	t.Run("missing expression", func(t *testing.T) {
		s := validSchedule()
		s.ScheduleExpression = ""
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "scheduleExpression is required")
	})
	t.Run("malformed expression", func(t *testing.T) {
		s := validSchedule()
		s.ScheduleExpression = "every minute"
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "scheduleExpression must look like")
	})
	t.Run("invalid timezone", func(t *testing.T) {
		s := validSchedule()
		s.ScheduleExpressionTimezone = "Mars/Olympus"
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "invalid IANA timezone")
	})
	t.Run("invalid state", func(t *testing.T) {
		s := validSchedule()
		s.State = "PAUSED"
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "state: must be ENABLED or DISABLED")
	})
	t.Run("invalid actionAfterCompletion", func(t *testing.T) {
		s := validSchedule()
		s.ActionAfterCompletion = "ARCHIVE"
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "actionAfterCompletion")
	})
	t.Run("non-RFC3339 dates", func(t *testing.T) {
		s := validSchedule()
		s.StartDate = "2026/05/05"
		s.EndDate = "tomorrow"
		errs := validateSchedule(s, "schedule[0]:x")
		wantSubstr(t, errs, "startDate: must be RFC3339")
		wantSubstr(t, errs, "endDate: must be RFC3339")
	})
	t.Run("flexible window mode invalid", func(t *testing.T) {
		s := validSchedule()
		s.FlexibleTimeWindow = &FlexibleTimeWindow{Mode: "SOFT"}
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "flexibleTimeWindow.mode")
	})
	t.Run("flexible mode requires window", func(t *testing.T) {
		s := validSchedule()
		s.FlexibleTimeWindow = &FlexibleTimeWindow{Mode: "FLEXIBLE"}
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "maximumWindowInMinutes: must be > 0")
	})
	t.Run("missing target", func(t *testing.T) {
		s := validSchedule()
		s.Target = nil
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "target: is required")
	})
	t.Run("target missing arn / roleArn", func(t *testing.T) {
		s := validSchedule()
		s.Target = &ScheduleTarget{}
		errs := validateSchedule(s, "schedule[0]:x")
		wantSubstr(t, errs, "arn: is required")
		wantSubstr(t, errs, "roleArn: is required")
	})
	t.Run("target arn not arn", func(t *testing.T) {
		s := validSchedule()
		s.Target.Arn = "lambda:f"
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "does not look like an ARN")
	})
	t.Run("target invalid input JSON", func(t *testing.T) {
		s := validSchedule()
		s.Target.Input = "{nope"
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "input: not valid JSON")
	})
	t.Run("target ecs invalid launch type", func(t *testing.T) {
		s := validSchedule()
		s.Target.EcsParameters = &SchedEcsParameters{
			TaskDefinitionArn: "arn:aws:ecs:us-east-1:1:task-definition/x:1",
			LaunchType:        "SERVERLESS",
		}
		wantSubstr(t, validateSchedule(s, "schedule[0]:x"), "ecsParameters.launchType")
	})
}

func TestValidateTag(t *testing.T) {
	cases := []struct {
		name, key, val, want string
	}{
		{"empty key", "", "v", "must be 1-128 chars"},
		{"too-long key", strings.Repeat("k", 129), "v", "must be 1-128 chars"},
		{"aws prefix", "aws:Created", "v", "cannot start with aws:"},
		{"value too long", "k", strings.Repeat("v", 257), "must be <=256 chars"},
		{"reserved tracking key", trackingTagKey, "v", "is reserved by ebschedule"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantSubstr(t, validateTag(tc.key, tc.val, "tags"), tc.want)
		})
	}
}

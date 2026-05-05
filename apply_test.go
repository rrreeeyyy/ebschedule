package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schtypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
)

// --- fakeEB ----------------------------------------------------------------

type fakeEBRule struct {
	rule    ebtypes.Rule
	targets []ebtypes.Target
	tags    map[string]string
}

type fakeEB struct {
	rules map[string]*fakeEBRule // by rule name

	deletedRules []string
	putRules     []string
	tagSet       map[string][]string // arn -> tag keys set
	tagUnset     map[string][]string // arn -> tag keys removed
}

func newFakeEB() *fakeEB {
	return &fakeEB{
		rules:    map[string]*fakeEBRule{},
		tagSet:   map[string][]string{},
		tagUnset: map[string][]string{},
	}
}

func (f *fakeEB) addRule(name string, tags map[string]string, targetIDs ...string) string {
	arn := "arn:aws:events:ap-northeast-1:1:rule/" + name
	tgts := make([]ebtypes.Target, 0, len(targetIDs))
	for _, id := range targetIDs {
		tgts = append(tgts, ebtypes.Target{
			Id:  aws.String(id),
			Arn: aws.String("arn:aws:lambda:ap-northeast-1:1:function:" + id),
		})
	}
	f.rules[name] = &fakeEBRule{
		rule: ebtypes.Rule{
			Name:               aws.String(name),
			Arn:                aws.String(arn),
			ScheduleExpression: aws.String("rate(1 hour)"),
			State:              ebtypes.RuleStateDisabled,
		},
		targets: tgts,
		tags:    tags,
	}
	return arn
}

func (f *fakeEB) ListRules(_ context.Context, in *eventbridge.ListRulesInput, _ ...func(*eventbridge.Options)) (*eventbridge.ListRulesOutput, error) {
	out := &eventbridge.ListRulesOutput{}
	names := make([]string, 0, len(f.rules))
	for n := range f.rules {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if in.NamePrefix != nil && !strings.HasPrefix(n, *in.NamePrefix) {
			continue
		}
		out.Rules = append(out.Rules, f.rules[n].rule)
	}
	return out, nil
}

func (f *fakeEB) ListTargetsByRule(_ context.Context, in *eventbridge.ListTargetsByRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.ListTargetsByRuleOutput, error) {
	r, ok := f.rules[*in.Rule]
	if !ok {
		return nil, &ebtypes.ResourceNotFoundException{}
	}
	return &eventbridge.ListTargetsByRuleOutput{Targets: r.targets}, nil
}

func (f *fakeEB) ListTagsForResource(_ context.Context, in *eventbridge.ListTagsForResourceInput, _ ...func(*eventbridge.Options)) (*eventbridge.ListTagsForResourceOutput, error) {
	for _, r := range f.rules {
		if aws.ToString(r.rule.Arn) == aws.ToString(in.ResourceARN) {
			out := &eventbridge.ListTagsForResourceOutput{}
			for k, v := range r.tags {
				out.Tags = append(out.Tags, ebtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
			}
			return out, nil
		}
	}
	return &eventbridge.ListTagsForResourceOutput{}, nil
}

func (f *fakeEB) DescribeRule(_ context.Context, in *eventbridge.DescribeRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error) {
	r, ok := f.rules[*in.Name]
	if !ok {
		return nil, &ebtypes.ResourceNotFoundException{}
	}
	return &eventbridge.DescribeRuleOutput{
		Name:               r.rule.Name,
		Arn:                r.rule.Arn,
		ScheduleExpression: r.rule.ScheduleExpression,
		State:              r.rule.State,
	}, nil
}

func (f *fakeEB) PutRule(_ context.Context, in *eventbridge.PutRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutRuleOutput, error) {
	name := *in.Name
	f.putRules = append(f.putRules, name)
	if _, ok := f.rules[name]; !ok {
		f.addRule(name, nil)
	}
	return &eventbridge.PutRuleOutput{RuleArn: f.rules[name].rule.Arn}, nil
}

func (f *fakeEB) PutTargets(_ context.Context, _ *eventbridge.PutTargetsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutTargetsOutput, error) {
	return &eventbridge.PutTargetsOutput{}, nil
}

func (f *fakeEB) RemoveTargets(_ context.Context, in *eventbridge.RemoveTargetsInput, _ ...func(*eventbridge.Options)) (*eventbridge.RemoveTargetsOutput, error) {
	r, ok := f.rules[*in.Rule]
	if !ok {
		return &eventbridge.RemoveTargetsOutput{}, nil
	}
	keep := r.targets[:0]
	rm := map[string]bool{}
	for _, id := range in.Ids {
		rm[id] = true
	}
	for _, t := range r.targets {
		if !rm[*t.Id] {
			keep = append(keep, t)
		}
	}
	r.targets = keep
	return &eventbridge.RemoveTargetsOutput{}, nil
}

func (f *fakeEB) TagResource(_ context.Context, in *eventbridge.TagResourceInput, _ ...func(*eventbridge.Options)) (*eventbridge.TagResourceOutput, error) {
	keys := make([]string, 0, len(in.Tags))
	for _, t := range in.Tags {
		keys = append(keys, aws.ToString(t.Key))
	}
	sort.Strings(keys)
	f.tagSet[aws.ToString(in.ResourceARN)] = keys
	return &eventbridge.TagResourceOutput{}, nil
}

func (f *fakeEB) UntagResource(_ context.Context, in *eventbridge.UntagResourceInput, _ ...func(*eventbridge.Options)) (*eventbridge.UntagResourceOutput, error) {
	keys := append([]string(nil), in.TagKeys...)
	sort.Strings(keys)
	f.tagUnset[aws.ToString(in.ResourceARN)] = keys
	return &eventbridge.UntagResourceOutput{}, nil
}

func (f *fakeEB) DeleteRule(_ context.Context, in *eventbridge.DeleteRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error) {
	delete(f.rules, *in.Name)
	f.deletedRules = append(f.deletedRules, *in.Name)
	return &eventbridge.DeleteRuleOutput{}, nil
}

// --- Rule prune tests ------------------------------------------------------

func TestApplyRulesWith_PruneRequiresTrackingID(t *testing.T) {
	f := newFakeEB()
	cfg := &Config{Rules: []*Rule{}}
	err := applyRulesWith(context.Background(), io.Discard, f, cfg, false, true)
	if err == nil || !strings.Contains(err.Error(), "trackingId") {
		t.Errorf("expected trackingId safety guard error, got %v", err)
	}
	if len(f.deletedRules) != 0 {
		t.Errorf("no deletes should happen when guard rejects: %v", f.deletedRules)
	}
}

func TestApplyRulesWith_PruneOnlyDeletesTracked(t *testing.T) {
	f := newFakeEB()
	f.addRule("mine-1", map[string]string{trackingTagKey: "my-app"}, "tgt")
	f.addRule("mine-2", map[string]string{trackingTagKey: "my-app"}, "tgt")
	f.addRule("foreign", map[string]string{"Owner": "terraform"}, "tgt")
	f.addRule("wrong-id", map[string]string{trackingTagKey: "different-app"}, "tgt")

	// cfg names mine-1 only; mine-2 is desired-out and should be pruned;
	// foreign and wrong-id must not be touched.
	cfg := &Config{
		TrackingID: "my-app",
		Rules: []*Rule{
			{
				Name:               "mine-1",
				ScheduleExpression: "rate(1 hour)",
				State:              "DISABLED",
				Targets:            []*Target{{ID: "tgt", Arn: "arn:aws:lambda:ap-northeast-1:1:function:tgt"}},
			},
		},
	}
	var out bytes.Buffer
	if err := applyRulesWith(context.Background(), &out, f, cfg, false, true); err != nil {
		t.Fatal(err)
	}
	sort.Strings(f.deletedRules)
	if !reflect.DeepEqual(f.deletedRules, []string{"mine-2"}) {
		t.Errorf("deletedRules = %v, want [mine-2] only", f.deletedRules)
	}
	if _, ok := f.rules["foreign"]; !ok {
		t.Error("foreign rule was deleted; safety violated")
	}
	if _, ok := f.rules["wrong-id"]; !ok {
		t.Error("rule with mismatched trackingId was deleted; safety violated")
	}
	if !strings.Contains(out.String(), "- rule:mine-2 (delete)") {
		t.Errorf("delete marker missing from output: %q", out.String())
	}
}

func TestApplyRulesWith_DryRunPruneSkipsDelete(t *testing.T) {
	f := newFakeEB()
	f.addRule("mine", map[string]string{trackingTagKey: "my-app"}, "tgt")
	cfg := &Config{TrackingID: "my-app", Rules: []*Rule{}}
	if err := applyRulesWith(context.Background(), io.Discard, f, cfg, true, true); err != nil {
		t.Fatal(err)
	}
	if len(f.deletedRules) != 0 {
		t.Errorf("dry-run should not call DeleteRule, got %v", f.deletedRules)
	}
	if _, ok := f.rules["mine"]; !ok {
		t.Error("dry-run mutated state")
	}
}

// --- fakeSched -------------------------------------------------------------

type fakeSched struct {
	// groupTags[group] = tags map
	groupTags map[string]map[string]string
	// schedules[group][name] = canonical view
	schedules map[string]map[string]*scheduler.GetScheduleOutput

	deletedSchedules []string
	createdGroups    []string
}

func newFakeSched() *fakeSched {
	return &fakeSched{
		groupTags: map[string]map[string]string{},
		schedules: map[string]map[string]*scheduler.GetScheduleOutput{},
	}
}

func (f *fakeSched) addGroup(name string, tags map[string]string) {
	f.groupTags[name] = tags
	if _, ok := f.schedules[name]; !ok {
		f.schedules[name] = map[string]*scheduler.GetScheduleOutput{}
	}
}

func (f *fakeSched) addSchedule(group, name string) {
	if _, ok := f.schedules[group]; !ok {
		f.schedules[group] = map[string]*scheduler.GetScheduleOutput{}
	}
	arn := fmt.Sprintf("arn:aws:scheduler:ap-northeast-1:1:schedule/%s/%s", group, name)
	f.schedules[group][name] = &scheduler.GetScheduleOutput{
		Name:               aws.String(name),
		Arn:                aws.String(arn),
		GroupName:          aws.String(group),
		ScheduleExpression: aws.String("rate(1 hour)"),
		State:              schtypes.ScheduleStateDisabled,
		FlexibleTimeWindow: &schtypes.FlexibleTimeWindow{Mode: schtypes.FlexibleTimeWindowModeOff},
		Target: &schtypes.Target{
			Arn:     aws.String("arn:aws:lambda:ap-northeast-1:1:function:" + name),
			RoleArn: aws.String("arn:aws:iam::1:role/r"),
		},
	}
}

func (f *fakeSched) GetScheduleGroup(_ context.Context, in *scheduler.GetScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.GetScheduleGroupOutput, error) {
	if _, ok := f.groupTags[*in.Name]; !ok {
		return nil, &schtypes.ResourceNotFoundException{}
	}
	return &scheduler.GetScheduleGroupOutput{
		Name: in.Name,
		Arn:  aws.String("arn:aws:scheduler:ap-northeast-1:1:schedule-group/" + *in.Name),
	}, nil
}

func (f *fakeSched) CreateScheduleGroup(_ context.Context, in *scheduler.CreateScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.CreateScheduleGroupOutput, error) {
	tags := map[string]string{}
	for _, t := range in.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.addGroup(*in.Name, tags)
	f.createdGroups = append(f.createdGroups, *in.Name)
	return &scheduler.CreateScheduleGroupOutput{}, nil
}

func (f *fakeSched) ListTagsForResource(_ context.Context, in *scheduler.ListTagsForResourceInput, _ ...func(*scheduler.Options)) (*scheduler.ListTagsForResourceOutput, error) {
	// Group ARN form: arn:...:schedule-group/<name>
	arn := aws.ToString(in.ResourceArn)
	prefix := "arn:aws:scheduler:ap-northeast-1:1:schedule-group/"
	if !strings.HasPrefix(arn, prefix) {
		return nil, fmt.Errorf("Scheduler tags only at the schedule-group level, got %q", arn)
	}
	name := strings.TrimPrefix(arn, prefix)
	out := &scheduler.ListTagsForResourceOutput{}
	for k, v := range f.groupTags[name] {
		out.Tags = append(out.Tags, schtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

func (f *fakeSched) ListSchedules(_ context.Context, in *scheduler.ListSchedulesInput, _ ...func(*scheduler.Options)) (*scheduler.ListSchedulesOutput, error) {
	out := &scheduler.ListSchedulesOutput{}
	for name, sched := range f.schedules[aws.ToString(in.GroupName)] {
		out.Schedules = append(out.Schedules, schtypes.ScheduleSummary{
			Name:      aws.String(name),
			GroupName: sched.GroupName,
			Arn:       sched.Arn,
		})
	}
	return out, nil
}

func (f *fakeSched) GetSchedule(_ context.Context, in *scheduler.GetScheduleInput, _ ...func(*scheduler.Options)) (*scheduler.GetScheduleOutput, error) {
	if g, ok := f.schedules[aws.ToString(in.GroupName)]; ok {
		if s, ok := g[aws.ToString(in.Name)]; ok {
			return s, nil
		}
	}
	return nil, &schtypes.ResourceNotFoundException{}
}

func (f *fakeSched) CreateSchedule(_ context.Context, _ *scheduler.CreateScheduleInput, _ ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error) {
	return &scheduler.CreateScheduleOutput{}, nil
}

func (f *fakeSched) UpdateSchedule(_ context.Context, _ *scheduler.UpdateScheduleInput, _ ...func(*scheduler.Options)) (*scheduler.UpdateScheduleOutput, error) {
	return &scheduler.UpdateScheduleOutput{}, nil
}

func (f *fakeSched) DeleteSchedule(_ context.Context, in *scheduler.DeleteScheduleInput, _ ...func(*scheduler.Options)) (*scheduler.DeleteScheduleOutput, error) {
	delete(f.schedules[aws.ToString(in.GroupName)], aws.ToString(in.Name))
	f.deletedSchedules = append(f.deletedSchedules, aws.ToString(in.Name))
	return &scheduler.DeleteScheduleOutput{}, nil
}

func (f *fakeSched) TagResource(_ context.Context, _ *scheduler.TagResourceInput, _ ...func(*scheduler.Options)) (*scheduler.TagResourceOutput, error) {
	return nil, errors.New("Scheduler.TagResource should not be called per-schedule")
}
func (f *fakeSched) UntagResource(_ context.Context, _ *scheduler.UntagResourceInput, _ ...func(*scheduler.Options)) (*scheduler.UntagResourceOutput, error) {
	return nil, errors.New("Scheduler.UntagResource should not be called per-schedule")
}

// --- Schedule prune tests --------------------------------------------------

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

func TestApplySchedulesWith_PruneSkipsUntrackedGroup(t *testing.T) {
	f := newFakeSched()
	// Group exists but lacks our tracking tag; treat as foreign-owned.
	f.addGroup("foreign-group", map[string]string{"Owner": "terraform"})
	f.addSchedule("foreign-group", "stranger")

	cfg := &Config{
		TrackingID: "my-app",
		GroupName:  "foreign-group",
		Schedules:  []*Schedule{},
	}

	stderr := captureStderr(t, func() {
		if err := applySchedulesWith(context.Background(), io.Discard, f, cfg, false, true); err != nil {
			t.Fatal(err)
		}
	})

	if len(f.deletedSchedules) != 0 {
		t.Errorf("must not delete schedules in untracked group, got %v", f.deletedSchedules)
	}
	if !strings.Contains(stderr, "skipping prune") {
		t.Errorf("expected skip-prune warning on stderr, got %q", stderr)
	}
	if _, ok := f.schedules["foreign-group"]["stranger"]; !ok {
		t.Error("stranger schedule was deleted; safety violated")
	}
}

func TestApplySchedulesWith_PruneDeletesUndesiredInTrackedGroup(t *testing.T) {
	f := newFakeSched()
	f.addGroup("my-group", map[string]string{trackingTagKey: "my-app"})
	f.addSchedule("my-group", "keep")
	f.addSchedule("my-group", "drop-me")

	cfg := &Config{
		TrackingID: "my-app",
		GroupName:  "my-group",
		Schedules: []*Schedule{
			{
				Name:               "keep",
				ScheduleExpression: "rate(1 hour)",
				State:              "DISABLED",
				FlexibleTimeWindow: &FlexibleTimeWindow{Mode: "OFF"},
				Target: &ScheduleTarget{
					Arn:     "arn:aws:lambda:ap-northeast-1:1:function:keep",
					RoleArn: "arn:aws:iam::1:role/r",
				},
			},
		},
	}
	if err := applySchedulesWith(context.Background(), io.Discard, f, cfg, false, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.deletedSchedules, []string{"drop-me"}) {
		t.Errorf("deletedSchedules = %v, want [drop-me] only", f.deletedSchedules)
	}
	if _, ok := f.schedules["my-group"]["keep"]; !ok {
		t.Error("kept schedule was deleted")
	}
}

func TestEnsureScheduleGroup_CreatesWithTrackingTag(t *testing.T) {
	f := newFakeSched()
	cfg := &Config{
		TrackingID: "my-app",
		Tags:       map[string]string{"Owner": "team"},
	}
	var out bytes.Buffer
	if err := ensureScheduleGroup(context.Background(), &out, f, "new-group", cfg, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.createdGroups, []string{"new-group"}) {
		t.Errorf("createdGroups = %v, want [new-group]", f.createdGroups)
	}
	tags := f.groupTags["new-group"]
	if tags[trackingTagKey] != "my-app" {
		t.Errorf("tracking tag missing on created group: %v", tags)
	}
	if tags["Owner"] != "team" {
		t.Errorf("Owner tag missing on created group: %v", tags)
	}
	if !strings.Contains(out.String(), "+ schedule-group:new-group (create)") {
		t.Errorf("create marker missing from output: %q", out.String())
	}
}

func TestEnsureScheduleGroup_NoOpForExisting(t *testing.T) {
	f := newFakeSched()
	f.addGroup("existing", nil)
	cfg := &Config{TrackingID: "my-app"}
	if err := ensureScheduleGroup(context.Background(), io.Discard, f, "existing", cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(f.createdGroups) != 0 {
		t.Errorf("should not re-create existing group, got %v", f.createdGroups)
	}
}

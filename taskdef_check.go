package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

// ecsDescribeAPI is the subset of *ecs.Client used by the task-def
// existence check. Defining it here lets tests inject a fake; the real
// *ecs.Client implements it implicitly.
type ecsDescribeAPI interface {
	DescribeTaskDefinition(ctx context.Context, in *ecs.DescribeTaskDefinitionInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
}

// verifyTaskDefinitions calls ecs:DescribeTaskDefinition for every distinct
// task-definition ARN referenced by the configs' ECS RunTask targets, so a
// typo or a deleted task definition fails before apply touches EventBridge
// rather than after a partial mutation. Mirrors what ecschedule does as
// part of its apply pipeline. Each ARN is described at most once, even if
// shared across rules / configs.
//
// On miss we surface the AWS error verbatim so the user sees the underlying
// message (often "ClientException: Unable to describe task definition" with
// the offending ARN), with our own prefix pointing at the rule that named
// it. Targets without ecsParameters are skipped — same goes for placeholder
// task-definition arns produced by validateFuncs (those never make it into
// online cmds, but defense-in-depth.)
func verifyTaskDefinitions(ctx context.Context, cli ecsDescribeAPI, cfgs []*Config) error {
	seen := map[string]error{}
	for _, c := range cfgs {
		for _, r := range c.Rules {
			for _, t := range r.Targets {
				if t.EcsParameters == nil {
					continue
				}
				arn := t.EcsParameters.TaskDefinitionArn
				if arn == "" || !strings.HasPrefix(arn, "arn:") {
					// Empty / template placeholder / unresolved
					// shorthand — nothing for AWS to describe.
					// expandShortcuts has already failed loudly on the
					// shorthand case, and validate flags empty values.
					continue
				}
				if cached, ok := seen[arn]; ok {
					if cached != nil {
						return fmt.Errorf("rule %s: taskDefinitionArn %s: %w", r.Name, arn, cached)
					}
					continue
				}
				_, err := cli.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
					TaskDefinition: aws.String(arn),
				})
				seen[arn] = err
				if err != nil {
					return fmt.Errorf("rule %s: taskDefinitionArn %s: %w", r.Name, arn, err)
				}
			}
		}
	}
	return nil
}

// newECSClientForRegion mirrors newEBClient/newScheduler clients elsewhere:
// build a real ecs.Client tied to a specific region. apply uses one client
// per region present in cfgs, so a multi-region config doesn't accidentally
// describe task defs against the wrong control plane.
func newECSClientForRegion(ctx context.Context, region string) (*ecs.Client, error) {
	awsCfg, err := loadAWS(ctx, region)
	if err != nil {
		return nil, err
	}
	return ecs.NewFromConfig(awsCfg), nil
}

// verifyTaskDefinitionsForCfgs is the apply-side entrypoint: groups configs
// by region, builds an ECS client per region, and runs the existence check.
// Skips configs whose Rules slice has zero ECS targets so we don't construct
// a client for nothing.
func verifyTaskDefinitionsForCfgs(ctx context.Context, cfgs []*Config) error {
	byRegion := map[string][]*Config{}
	for _, c := range cfgs {
		if !hasECSTarget(c) {
			continue
		}
		byRegion[c.Region] = append(byRegion[c.Region], c)
	}
	for region, group := range byRegion {
		cli, err := newECSClientForRegion(ctx, region)
		if err != nil {
			return fmt.Errorf("ecs client (region=%q): %w", region, err)
		}
		if err := verifyTaskDefinitions(ctx, cli, group); err != nil {
			return err
		}
	}
	return nil
}

func hasECSTarget(c *Config) bool {
	for _, r := range c.Rules {
		for _, t := range r.Targets {
			if t.EcsParameters != nil {
				return true
			}
		}
	}
	return false
}

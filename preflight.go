package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// loadAWS builds a per-region SDK config using the standard credential
// chain (env / shared config file / IMDS / OIDC role assumption set up
// by the workflow). Every per-service client constructor in the codebase
// flows through here.
func loadAWS(ctx context.Context, region string) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// resolveSTSAccount fetches the calling account ID via
// sts:GetCallerIdentity using whatever credentials the SDK chain
// resolves. Region is irrelevant to GetCallerIdentity (global call), so
// we pass "" to loadAWS.
func resolveSTSAccount(ctx context.Context) (string, error) {
	awsCfg, err := loadAWS(ctx, "")
	if err != nil {
		return "", err
	}
	cli := sts.NewFromConfig(awsCfg)
	out, err := cli.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Account), nil
}

// stsAccountResolver is the indirection used by autoResolveAccountEnv.
// Production points at resolveSTSAccount; tests swap it via
// swapSTSResolver in main_test.go to avoid standing up a real AWS
// client.
var stsAccountResolver = resolveSTSAccount

// autoResolveAccountEnv populates AWS_ACCOUNT_ID from sts:GetCallerIdentity
// when the env var is empty. Online subcommands (diff/apply/dump/run) call
// this before loadConfigs so configs that omit a top-level `account:` (the
// common ecschedule shape) still resolve ARN shorthand correctly.
//
// On STS failure we leave the env var unset and continue silently:
// expandShortcuts surfaces a clear "shorthand requires account" error if the
// value is actually needed, and the upcoming preflightCheck will report any
// real credential problem with full context. Calling it from validate /
// import-ecschedule would defeat the offline guarantee, so they don't.
func autoResolveAccountEnv(ctx context.Context) {
	if os.Getenv("AWS_ACCOUNT_ID") != "" {
		return
	}
	id, err := stsAccountResolver(ctx)
	if err != nil || id == "" {
		return
	}
	_ = os.Setenv("AWS_ACCOUNT_ID", id)
}

// preflightCheck verifies AWS credentials by calling sts:GetCallerIdentity
// once per region present in cfgs (deduplicated). It runs before any
// mutation so an apply doesn't get half-way then trip on expired SSO.
// Errors here are surfaced with the AWS error wrapped so the user sees
// the underlying cause directly.
func preflightCheck(ctx context.Context, cfgs []*Config) error {
	seen := map[string]bool{}
	regions := []string{}
	for _, c := range cfgs {
		if seen[c.Region] {
			continue
		}
		seen[c.Region] = true
		regions = append(regions, c.Region)
	}
	for _, region := range regions {
		awsCfg, err := loadAWS(ctx, region)
		if err != nil {
			return fmt.Errorf("AWS credentials (region=%q): %w", region, err)
		}
		cli := sts.NewFromConfig(awsCfg)
		if _, err := cli.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
			return fmt.Errorf("sts:GetCallerIdentity (region=%q): %w", region, err)
		}
	}
	return nil
}

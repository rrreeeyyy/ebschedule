// 11-jsonnet/conf.jsonnet: jsonnet alternative to YAML, picked up by the
// `.jsonnet` (or `.libsonnet`) extension.
//
// Native funcs (parallel to YAML's template funcs):
//
//   std.native('env')(name, default)     // env or default
//   std.native('must_env')(name)         // env or error
//   std.native('ssm')(name)              // SSM Parameter Store, decrypted
//   std.native('ssmList')(name)          // SSM StringList split on commas, returned as array
//   std.native('tfstate')(resource)      // tfstate lookup (EBSCHEDULE_TFSTATE_URL)
//   std.native('tfstatef')(fmt, args...) // tfstate sprintf-style helper
//
// Under `validate`, must_env / ssm / tfstate fall back to placeholders
// (`<env:NAME>` / `<ssm:/path>` / `<tfstate:resource>`) so the example
// validates offline without any env or AWS access:
//
//   ebschedule -conf examples/11-jsonnet/conf.jsonnet validate
//
// For apply / diff, set the env vars (or rely on STS auto-detect for
// AWS_ACCOUNT_ID).

local account = std.native('must_env')('AWS_ACCOUNT_ID');
local region = std.native('env')('AWS_REGION', 'ap-northeast-1');
local cluster = 'examples-cluster';

local commonTarget = {
  id: 'ecs',
  arn: 'arn:aws:ecs:%s:%s:cluster/%s' % [region, account, cluster],
  roleArn: 'arn:aws:iam::%s:role/ecsEventsRole' % [account],
};

local nightly(name, expr, taskDef) = {
  name: name,
  scheduleExpression: expr,
  state: 'ENABLED',
  targets: [
    commonTarget {
      ecsParameters: {
        taskDefinitionArn: 'arn:aws:ecs:%s:%s:task-definition/%s' % [region, account, taskDef],
        launchType: 'FARGATE',
        subnets: ['subnet-aaa', 'subnet-bbb'],
        securityGroups: ['sg-egress'],
        assignPublicIp: 'DISABLED',
      },
    },
  ],
};

{
  region: region,
  trackingId: 'examples-11',
  tags: {
    Owner: 'examples',
    GeneratedBy: 'jsonnet',
  },
  rules: [
    nightly('example-nightly-etl', 'cron(0 18 * * ? *)', 'etl'),
    nightly('example-nightly-rollup', 'cron(0 19 * * ? *)', 'rollup'),
  ],
}

// 11-jsonnet/conf.jsonnet: jsonnet alternative to YAML, picked up by the
// `.jsonnet` (or `.libsonnet`) extension.
//
// Native funcs (parallel to YAML's template funcs):
//
//   std.native('env')(name, default)     // env or default
//   std.native('must_env')(name)         // env or error
//   std.native('ssm')(name)              // SSM Parameter Store, decrypted
//   std.native('tfstate')(resource)      // tfstate lookup (EBSCHEDULE_TFSTATE_URL)
//   std.native('tfstatef')(fmt, args...) // tfstate sprintf-style helper
//
//   AWS_ACCOUNT_ID=123 ebschedule -conf examples/11-jsonnet/conf.jsonnet validate

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

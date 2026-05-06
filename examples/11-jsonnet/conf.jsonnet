// 11-jsonnet/conf.jsonnet: jsonnet alternative to YAML, picked up by the
// `.jsonnet` (or `.libsonnet`) extension. Useful when the config grows
// expressions, list comprehensions, or shared snippets across stages.
//
// Env vars come in via the native funcs `env(name, default)` and
// `must_env(name)` (matching ecspresso's convention). `local` bindings,
// imports, and arithmetic give you the full programming model that pure
// YAML lacks.
//
//   AWS_ACCOUNT_ID=123 ebschedule -conf examples/11-jsonnet/conf.jsonnet validate

local account = std.native('must_env')('AWS_ACCOUNT_ID');
local region = 'ap-northeast-1';
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

# Examples

Each YAML here is a self-contained config that demonstrates one capability.
The examples reference `{{ must_env "AWS_ACCOUNT_ID" }}` (or jsonnet's
`std.native('must_env')`) and pass `ebschedule validate` offline — under
`validate`, both paths fall back to a `<env:AWS_ACCOUNT_ID>` placeholder
that satisfies the ARN check, so no env vars or AWS credentials are
required for the structural pass. For `apply` / `diff` / `dump` / `run`,
ebschedule auto-fills `AWS_ACCOUNT_ID` from `sts:GetCallerIdentity` if
the env var is unset, so you don't need to export it explicitly when
running against the calling account.

```sh
# Walk through any example without hitting AWS:
ebschedule -conf examples/03-schedule-cron-timezone.yaml validate

# Run for real against the calling account (account auto-resolved via STS):
AWS_PROFILE=my-sandbox \
  ebschedule -conf examples/03-schedule-cron-timezone.yaml apply

# Or pin AWS_ACCOUNT_ID explicitly for cross-account or CI:
AWS_PROFILE=my-sandbox AWS_ACCOUNT_ID=123456789012 \
  ebschedule -conf examples/03-schedule-cron-timezone.yaml apply
```

| File | Demonstrates |
| --- | --- |
| [01-rule-lambda.yaml](./01-rule-lambda.yaml) | Smallest possible config: a Rule firing a Lambda on a rate schedule |
| [02-rule-event-pattern.yaml](./02-rule-event-pattern.yaml) | `eventPattern` against S3 events; Step Functions target with retry / DLQ |
| [03-schedule-cron-timezone.yaml](./03-schedule-cron-timezone.yaml) | EventBridge Scheduler with timezone-aware cron + a one-shot `at(...)` schedule |
| [04-multi-group-schedules.yaml](./04-multi-group-schedules.yaml) | Per-schedule `groupName:` override; one config managing several groups |
| [05-ecs-runtask.yaml](./05-ecs-runtask.yaml) | ECS Fargate RunTask via Rule + `containerOverrides` (the ecschedule pattern) |
| [06-target-types.yaml](./06-target-types.yaml) | Variety pack: Kinesis, SQS FIFO, Batch, Redshift Data, SageMaker pipeline, API Destination |
| [07-template-funcs.yaml](./07-template-funcs.yaml) | `env` / `must_env` / `ssm` template substitution |
| [08-cluster-shorthand.yaml](./08-cluster-shorthand.yaml) | ecschedule-style top-level `cluster:` / `account:` shorthand; short names auto-expand to full ARNs |
| [09-base-file/](./09-base-file/) | `baseFile:` inheritance: shared region / account / cluster / tags in `_base.yaml`, env-specific rules in `prod.yaml` / `staging.yaml` |
| [10-tfstate.yaml](./10-tfstate.yaml) | `{{ tfstate "..." }}` lookup against a Terraform state file via `EBSCHEDULE_TFSTATE_URL` |
| [11-jsonnet/](./11-jsonnet/) | Jsonnet config (`.jsonnet` / `.libsonnet`); env / SSM / tfstate via `std.native('env'\|'must_env'\|'ssm'\|'tfstate')`, imports, comprehensions |
| [multi-file/](./multi-file/) | Glob-loaded configs: `-conf 'examples/multi-file/*.yaml'`; per-file trackingId keeps prune scoped per team |

`_base.yaml` style files are referenced via `baseFile:`; they're never loaded
directly. Don't glob include them in `-conf '...*.yaml'` patterns - load only
the leaf configs:

```sh
ebschedule -conf examples/09-base-file/prod.yaml apply
ebschedule -conf examples/09-base-file/staging.yaml apply
```

## Bootstrapping from existing AWS

If you already have rules in an account, dump them filtered by tag and use
the output as a starting point:

```sh
ebschedule -conf /dev/null \
  -tag Service=my-app \
  -tag Env=prod \
  dump > my-app.yaml
```

The `/dev/null` config tells ebschedule "don't read a config file, just hit
AWS." See the dump section in the top-level README for more.

## ecschedule migration

The `import-ecschedule` subcommand converts an [ecschedule](https://github.com/Songmu/ecschedule)
YAML to ebschedule format:

```sh
ebschedule import-ecschedule \
  -in path/to/ecschedule.yaml \
  -account 123456789012 \
  -tracking-id my-app \
  > my-app.yaml
ebschedule -conf my-app.yaml validate
```

`05-ecs-runtask.yaml` shows the kind of output you can expect.

# ebschedule

Declarative CLI for managing **Amazon EventBridge Rules** and **EventBridge
Scheduler Schedules** from a single YAML config. Inspired by
[Songmu/ecschedule](https://github.com/Songmu/ecschedule); generalized to
arbitrary target types (not just ECS RunTask) and to both EventBridge
services.

One binary, one YAML, one CLI — `dump` / `diff` / `apply` / `validate` plus
an `import-ecschedule` converter.

## Install

```sh
go install github.com/rrreeeyyy/ebschedule@latest
```

Or build from source:

```sh
go build -o ebschedule .
```

### GitHub Actions

A composite action installs ebschedule from the published release for
the runner's OS/arch and adds it to `PATH`:

```yaml
jobs:
  apply:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: rrreeeyyy/ebschedule@v1
        with:
          version: v0.1.0          # or "latest" (default)
      - run: ebschedule -conf config/ -prune apply
        env:
          AWS_ACCOUNT_ID: ${{ vars.AWS_ACCOUNT_ID }}
```

Inputs: `version` (release tag or `latest`), `github-token` (defaults
to `github.token`), `install-dir` (defaults to `/usr/local/bin`).

## Quick start

ebschedule covers two AWS services in one config:

- `rules:` — **EventBridge Rules** (the older bus-attached service).
  Triggered by `scheduleExpression` (cron / rate) **or** `eventPattern`
  (events on a bus). Wide target set: ECS RunTask, Lambda, Step
  Functions, SQS, SNS, Kinesis, Batch, API Destination, …
- `schedules:` — **EventBridge Scheduler** (the newer time-only
  service). cron / rate / one-shot `at(...)`, timezone-aware
  (`timezone:`), with schedule groups and the
  universal AWS-SDK target. No `eventPattern` — time triggers only.

A single YAML can manage both, and `dump` / `diff` / `apply` operate on
whichever is present. Minimal example covering both:

```yaml
# ebschedule.yaml
region: ap-northeast-1
trackingId: my-app

# EventBridge Rules: scheduleExpression OR eventPattern
rules:
  - name: nightly-etl
    scheduleExpression: cron(0 18 * * ? *)
    state: ENABLED
    targets:
      - id: lambda
        arn: arn:aws:lambda:ap-northeast-1:{{ must_env "AWS_ACCOUNT_ID" }}:function:etl

# EventBridge Scheduler: time-only, timezone-aware
schedules:
  - name: weekly-rollup
    scheduleExpression: cron(0 9 ? * MON *)
    timezone: Asia/Tokyo
    state: ENABLED
    target:
      arn: arn:aws:lambda:ap-northeast-1:{{ must_env "AWS_ACCOUNT_ID" }}:function:rollup
      roleArn: arn:aws:iam::{{ must_env "AWS_ACCOUNT_ID" }}:role/SchedulerInvokeLambda
```

ECS RunTask gets its own ecschedule-compatible shorthand — see
[ECS RunTask shorthand](#ecs-runtask-shorthand-ecschedule-compatible).

```sh
# bootstrap a config from what's already in AWS
ebschedule dump my-app- > ebschedule.yaml

# bootstrap by tag (e.g. only Rules tagged Service=my-app, AND'd if multiple)
ebschedule -tag Service=my-app -tag Env=prod dump > ebschedule.yaml

# offline structural check (no AWS calls)
ebschedule -conf ebschedule.yaml validate

# preview what would change
ebschedule -conf ebschedule.yaml diff

# apply (dry-run, then real)
ebschedule -conf ebschedule.yaml -dry-run apply
ebschedule -conf ebschedule.yaml apply

# apply + prune resources you previously managed but removed from config
ebschedule -conf ebschedule.yaml -prune apply

# multi-file (e.g. one per service / team)
ebschedule -conf 'config/*.yaml' -prune apply
```

Note: flags must precede the subcommand. `-prune apply` works;
`apply -prune` silently drops the flag.

See [`ebschedule.yaml`](./ebschedule.yaml) for an example covering both
Rules and Schedules.

## Subcommands

| Command             | Reads AWS | Mutates AWS | Notes                                              |
| ------------------- | :-------: | :---------: | -------------------------------------------------- |
| `validate`          |     —     |      —      | Offline structural check; exits non-zero on errors |
| `dump [prefix]`     |     ✓     |      —      | Emit YAML reflecting current AWS state             |
| `diff`              |     ✓     |      —      | Unified-diff per resource; exits 2 on drift        |
| `apply`             |     ✓     |      ✓      | Create / update; `-dry-run` keeps it read-only. Pre-flight verifies AWS creds + every referenced ECS task definition exists |
| `-prune apply`      |     ✓     |      ✓      | Apply + delete tracked resources missing from config |
| `run -rule NAME`    |     ✓     |      ✓      | Invoke a rule's targets right now (ECS / Lambda / SFN); `-dry-run` skips AWS |
| `import-ecschedule` |     —     |      —      | Convert an ecschedule YAML to ebschedule format    |

## Config semantics: omitted vs. empty

| YAML state              | Behavior                                                       |
| ----------------------- | -------------------------------------------------------------- |
| `rules:` omitted        | ebschedule never touches Rules (incl. no prune)                |
| `rules: []`             | ebschedule manages Rules; with `-prune`, deletes every tracked |
| `rules: [...items]`     | ebschedule manages exactly those items                         |
| `schedules:` — same     | same                                                           |

This lets one tool manage Rules without disturbing Schedules (and vice
versa), or split ownership across multiple config files.

## Tagging model

| Resource              | Where tags live       | Source in YAML                                      |
| --------------------- | --------------------- | --------------------------------------------------- |
| EventBridge Rule      | per-rule              | top-level `tags:` ∪ per-rule `tags:` (latter wins)  |
| Scheduler Schedule    | none (API limitation) | —                                                   |
| Scheduler Group       | per-group             | top-level `tags:` (set at group create)             |

EventBridge Scheduler exposes tags only at the schedule-group level, so
ebschedule tags the group itself. There's no per-schedule `tags:` field.

On `apply`, ebschedule reconciles Rule tags fully: adds missing, removes
remote tags absent from desired. Existing schedule groups are **never**
mutated (ebschedule only sets tags when creating a group), so groups
shared with Terraform / CDK aren't disturbed.

## Prune safety

`-prune` is scoped via the `ebschedule-tracking-id` tag.

- A `trackingId:` is **required** in YAML when using `-prune`; without it
  prune is rejected.
- For **Rules**: only Rules whose tag matches the configured `trackingId`
  are eligible. Resources created by other tools (Terraform, CDK, console)
  remain untouched.
- For **Schedules**: ebschedule scans every schedule-group in the account
  whose tags include `ebschedule-tracking-id=<your-id>` and prunes
  schedules within those groups. Foreign groups (no tracking tag, or a
  different value) are never visited, so groups shared with other tools
  are safe. With per-schedule `groupName:` override, removing a schedule
  from the config also cleans up schedules left in groups the config no
  longer references.

A typical pattern:

```yaml
trackingId: my-app   # any stable string
groupName: my-app    # ebschedule-owned group (auto-created on first apply)
```

## Templating

Config files run through `text/template` **before** YAML parsing:

| Func                              | Notes                                                                                          |
| --------------------------------- | ---------------------------------------------------------------------------------------------- |
| `{{ env "X" }}`                   | Empty string if `X` is unset                                                                   |
| `{{ must_env "X" }}`              | Errors out (or placeholder under `validate`)                                                   |
| `{{ ssm "/p/k" }}`                | SSM Parameter Store, decrypted, region from AWS creds                                          |
| `{{ tfstate "type.name.attr" }}`  | Terraform state lookup; needs `EBSCHEDULE_TFSTATE_URL` env                                     |
| `{{ tfstatef "type.%s.attr" x }}` | sprintf-style tfstate helper from `fujiwara/tfstate-lookup`; same `EBSCHEDULE_TFSTATE_URL` env |

Under `validate`, AWS / tfstate is never called: `ssm` returns
`<ssm:/path>`, `tfstate` returns `<tfstate:type.name.attr>`, and
`must_env` falls back to `<env:NAME>` so the structural check works
offline. Target ARN validation accepts those placeholders so a config
pulling ARNs from tfstate validates without the URL set.

`EBSCHEDULE_TFSTATE_URL` accepts a local path, `s3://`, `http(s)://`,
or a Terraform Cloud (`remote://`) workspace. GCS and Azurerm backends
are intentionally excluded from release builds (`-tags=no_gcs,no_azurerm`)
to slim the binary; rebuild without those tags if you need them. See
[examples/10-tfstate.yaml](./examples/10-tfstate.yaml).

## Diff

Unified-diff (git-style) per resource, comparing remote vs desired YAML.

For Schedules, the comparison strips Scheduler's documented defaults
(`timezone: UTC`, `actionAfterCompletion: NONE`, `retryPolicy: {185, 86400}`)
on both sides, so a YAML that explicitly writes those defaults still
shows as no-op.

The internal `ebschedule-tracking-id` tag is hidden from diff.

## ECS RunTask shorthand (ecschedule-compatible)

Top-level `region:`, `account:`, and `cluster:` enable short names inside
ECS RunTask targets:

```yaml
region: ap-northeast-1
account: '{{ must_env "AWS_ACCOUNT_ID" }}'
cluster: my-cluster

rules:
  - name: nightly
    scheduleExpression: cron(0 18 * * ? *)
    targets:
      - id: ecs
        # arn: omitted -> arn:aws:ecs:{region}:{account}:cluster/{cluster}
        roleArn: ecsEventsRole          # -> arn:aws:iam::{account}:role/ecsEventsRole
        ecsParameters:
          taskDefinitionArn: my-batch   # -> arn:aws:ecs:{region}:{account}:task-definition/my-batch
          launchType: FARGATE
```

Anything already starting with `arn:` is left unchanged, so migration
from ecschedule is incremental — flip whichever fields you want to short
names; the rest can stay full ARN.

`account:` defaults to `AWS_ACCOUNT_ID` env when omitted; if neither is
set, online subcommands (`diff` / `apply` / `dump` / `run`) fill it in
automatically via `sts:GetCallerIdentity` using the same credentials
they'll use for the rest of the call. So a single config can be reused
across accounts without explicit account wiring (matches ecschedule's
behavior). `validate` and `import-ecschedule` skip the STS lookup so
they stay offline. See
[examples/08-cluster-shorthand.yaml](./examples/08-cluster-shorthand.yaml).

## `baseFile:` config inheritance

Share scaffolding (region / account / cluster / groupName /
eventBusName / trackingId / tags) across multiple ebschedule yamls
without copy-pasting:

```yaml
# _base.yaml — shared scaffolding only, no rules: / schedules:
region: ap-northeast-1
account: '{{ must_env "AWS_ACCOUNT_ID" }}'
cluster: my-cluster
tags:
  Service: my-app
```

```yaml
# prod.yaml — pulls scaffolding from _base.yaml; child wins on conflict
baseFile: _base.yaml
trackingId: my-app-prod
tags: { Env: prod }
rules:
  - name: nightly
    scheduleExpression: cron(0 18 * * ? *)
    targets:
      - id: ecs
        roleArn: ecsEventsRole          # cluster-shorthand still works
        ecsParameters:
          taskDefinitionArn: my-app-prod-batch
          launchType: FARGATE
```

`baseFile:` is resolved relative to the child's path. Tags merge (child
overrides on conflict); scalars inherit only when the child left them
empty. Recursive parents (parent has its own `baseFile:`) are supported;
cycles are detected and rejected.

A baseFile may not declare `rules:` or `schedules:` — they belong in
the leaf file. Files referenced via `baseFile:` are never loaded
directly, so don't glob include them in `-conf '...*.yaml'` patterns.

See [examples/09-base-file/](./examples/09-base-file).

## Jsonnet

Files ending in `.jsonnet` or `.libsonnet` are evaluated with
[google/go-jsonnet](https://github.com/google/go-jsonnet) before the
result is parsed as Config. Useful when the config grows expressions,
list comprehensions, or shared snippets across stages.

```jsonnet
local account = std.native('must_env')('AWS_ACCOUNT_ID');
local nightly(name, td) = {
  name: name,
  scheduleExpression: 'cron(0 18 * * ? *)',
  state: 'ENABLED',
  targets: [{
    id: 'ecs',
    arn: 'arn:aws:ecs:ap-northeast-1:%s:cluster/c' % [account],
    roleArn: 'arn:aws:iam::%s:role/ecsEventsRole' % [account],
    ecsParameters: {
      taskDefinitionArn: 'arn:aws:ecs:ap-northeast-1:%s:task-definition/%s' % [account, td],
      launchType: 'FARGATE',
    },
  }],
};

{
  region: 'ap-northeast-1',
  trackingId: 'my-app',
  rules: [nightly('etl', 'etl'), nightly('rollup', 'rollup')],
}
```

Native funcs parallel the YAML+template pipeline so the same kind of
config can be written in either format:

| Native | Equivalent in YAML |
| --- | --- |
| `std.native('env')(name, default)` | `{{ env "NAME" }}` |
| `std.native('must_env')(name)` | `{{ must_env "NAME" }}` |
| `std.native('ssm')(name)` | `{{ ssm "/path" }}` |
| `std.native('tfstate')(resource)` | `{{ tfstate "type.name.attr" }}` |

`std.extVar` is left for explicit user-supplied values. Local imports
(`import './lib.libsonnet'`) resolve relative to the file.

Text/template (`env` / `must_env` / `ssm` / `tfstate`) is **not**
applied to jsonnet input — jsonnet has its own machinery via the
natives above.

See [examples/11-jsonnet/](./examples/11-jsonnet).

## Strict YAML

Unknown fields fail with a line-numbered error rather than being
silently dropped. A typo like `tag:` instead of `tags:` is caught at
load time across `validate` / `dump` / `diff` / `apply`.

## JSON-shaped fields

`Rule.eventPattern` and `target.input` are JSON documents on the AWS
side. ebschedule accepts either form in YAML:

```yaml
# structured (recommended; readable, no escaping)
eventPattern:
  source:
    - aws.s3
  detail-type:
    - Object Created

# legacy raw-JSON-string (still supported)
eventPattern: '{"source":["aws.s3"],"detail-type":["Object Created"]}'
```

Internally, both forms are normalized to canonical JSON (compact, sorted
keys) so `diff` is whitespace- and key-order-insensitive between user
input and what AWS returns. `dump` emits the structured form on the way
out, so a `dump | edit | apply` round-trip stays readable.

## Multi-file configs

`-conf` accepts a glob (`-conf 'config/*.yaml'`). Each matched file is
loaded as an independent `Config`. Useful for splitting per-service
ownership while keeping prune scopes isolated.

## `import-ecschedule`

Convert an existing ecschedule YAML to ebschedule format:

```sh
ebschedule import-ecschedule -in ecschedule.yaml -account 123456789012 \
  -tracking-id my-app > ebschedule.yaml
```

- ECS RunTask targets are emitted as EventBridge Rule targets with
  `ecsParameters`.
- `containerOverrides` and `taskOverride` are encoded into the target's
  `input` JSON; `run` decodes them again on dispatch so ad-hoc
  invocation keeps the same overrides.
- IAM role resolution mirrors ecschedule's chain: per-target `role`
  wins, falling back to the top-level `role:`, then to `ecsEventsRole`
  — so a config that omits the role in one or both spots still imports
  with a non-empty `roleArn` (whatever ecschedule would have used at
  apply time).
- A `plugins:` block (e.g. ecschedule's tfstate-lookup plugin) is
  surfaced as a stderr warning and dropped from the output. ebschedule
  reads tfstate via the `EBSCHEDULE_TFSTATE_URL` env var; set it before
  running `apply` / `diff`.
- If `-account` is not given and `AWS_ACCOUNT_ID` is unset, the converter
  emits a `{{ must_env "AWS_ACCOUNT_ID" }}` placeholder so a single
  config can be reused across accounts.

## Worked examples

The [`examples/`](./examples) directory has focused YAMLs for common
shapes — Lambda / Step Functions / ECS RunTask / Kinesis / SQS FIFO /
Batch / Redshift / SageMaker / API Destination, plus multi-group
schedules and multi-file glob layouts. Each one passes `ebschedule
validate` straight out of the box. See the
[examples README](./examples/README.md) for the full index.

## Walkthroughs

### Migrating from ecschedule

If you already maintain an `ecschedule.yaml` for ECS Scheduled Tasks,
the converter emits a drop-in replacement:

```sh
ebschedule import-ecschedule \
  -in path/to/ecschedule.yaml \
  -account 123456789012 \
  -tracking-id my-app \
  > my-app.yaml

ebschedule -conf my-app.yaml validate     # offline structural check
ebschedule -conf my-app.yaml diff          # vs current AWS state
ebschedule -conf my-app.yaml apply         # confirms before mutating
```

`-account` is optional; if omitted (and `AWS_ACCOUNT_ID` is unset), the
converter emits `{{ must_env "AWS_ACCOUNT_ID" }}` so a single config
runs across accounts. ECS RunTask `containerOverrides` are encoded into
the EventBridge target's `input` JSON. See
[examples/05-ecs-runtask.yaml](./examples/05-ecs-runtask.yaml) for the
shape of the output.

### Bootstrapping a config from an existing AWS account

`dump` emits the YAML form of whatever it finds; combine with
`-tag KEY=VALUE` to scope the bootstrap to resources you actually own:

```sh
# Pull every Rule tagged Service=my-app and Env=prod into a starter config:
ebschedule \
  -tag Service=my-app \
  -tag Env=prod \
  dump > my-app.yaml

# Edit my-app.yaml as needed (most importantly: pick a trackingId), then
# round-trip with diff to confirm there's nothing to change:
$EDITOR my-app.yaml
ebschedule -conf my-app.yaml diff && echo "in sync"
```

Multiple `-tag` flags are AND-matched. Schedules dump per-group as
before; the `-tag` filter applies to Rules only. `dump` tolerates a
missing `ebschedule.yaml` (the default `-conf` path) — when there's
nothing to load, it just emits a fresh YAML based on what it finds in
AWS, so the bootstrap call doesn't need any config-shaped scaffolding.

### CI: gate PRs on drift

`diff` exits with code `2` when there's drift (terraform-plan style),
`0` when clean, `1` on error. A typical pipeline step:

```yaml
# .github/workflows/check-drift.yml
- run: ebschedule -conf 'config/*.yaml' diff
```

When `apply` is part of CI, pass `-auto-approve` to skip the
interactive confirmation:

```yaml
- run: ebschedule -conf 'config/*.yaml' -auto-approve apply
```

### Operating on a single resource

`-target KIND:NAME` restricts both `diff` and `apply` to specific
resources, useful for surgical rollouts:

```sh
ebschedule -conf my-app.yaml -target rule:nightly-batch diff
ebschedule -conf my-app.yaml \
  -target schedule:hourly-sync \
  -target schedule:daily-sync \
  apply
```

`-target` and `-prune` are mutually exclusive. Naming a resource the
config doesn't define errors out rather than silently skipping.

### Running a rule on demand

`run -rule NAME` invokes a rule's targets immediately, without waiting
for its `scheduleExpression`. Useful for re-running a missed nightly
batch or for smoke-testing a new rule:

```sh
ebschedule -conf ebschedule.yaml run -rule example-nightly-etl
ebschedule -conf ebschedule.yaml -dry-run run -rule example-nightly-etl
```

Dispatch is by target ARN: `arn:aws:ecs:.../cluster/...` (with
`ecsParameters`) calls `ecs:RunTask`, `arn:aws:lambda:.../function:...`
calls `lambda:Invoke` (passing `target.input` as the payload, defaulting
to `{}`), and `arn:aws:states:.../stateMachine:...` calls
`sfn:StartExecution`. Other target types error with a clear "not a
supported invocation type" message.

For ECS targets, when `target.input` carries the ecschedule-shaped
`{containerOverrides, taskOverride}` payload (the same JSON
`import-ecschedule` writes), `run` translates it into RunTask
`Overrides` so `run` matches what ecschedule's `run` does — same
container Command / Environment / Cpu / Memory and task-level Cpu /
Memory take effect.

The flags mirror ecschedule's: `-rule NAME` is required, `-dry-run`
prints what would be invoked without calling AWS, and the global
`-conf` selects the config file.

### Splitting per-team or per-service

When several teams share an account, give each team its own config file
(and its own `trackingId`). Glob loads them all in one invocation but
keeps the prune scope per-file:

```sh
ebschedule -conf 'config/*.yaml' -prune apply
```

See [examples/multi-file/](./examples/multi-file) for a worked layout
(shared Rule + per-team schedule groups).

## Files

- `main.go` — CLI dispatch, `Config`, template / SSM helpers, tag reconciliation
- `rule.go` — EventBridge Rule operations
- `schedule.go` — EventBridge Scheduler operations + group auto-create
- `validate.go` — offline structural validation
- `import.go` — ecschedule → ebschedule converter
- `ebschedule.yaml` — example covering Rules + Schedules
- `examples/` — focused per-feature configs (see [examples/README.md](./examples/README.md))

## Extend

To support more target shapes, add a field to the relevant
`Target` / `ScheduleTarget`, copy in `fromRemote*` (read), and copy in
`toAWS*` (write). The pattern is small and consistent across both
services.

## License

MIT. See [LICENSE](./LICENSE).

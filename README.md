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

## Quick start

```sh
# bootstrap a config from what's already in AWS
ebschedule dump my-app- > ebschedule.yaml

# bootstrap by tag (e.g. only Rules tagged Service=my-app, AND'd if multiple)
ebschedule -conf /dev/null -tag Service=my-app -tag Env=prod dump > ebschedule.yaml

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
ebschedule -conf 'config/*.yaml' apply -prune
```

See [`ebschedule.yaml`](./ebschedule.yaml) for an example covering both
Rules and Schedules.

## Subcommands

| Command              | Reads AWS | Mutates AWS | Notes                                              |
| -------------------- | :-------: | :---------: | -------------------------------------------------- |
| `validate`           |     —     |      —      | Offline structural check; exits non-zero on errors |
| `dump [prefix]`      |     ✓     |      —      | Emit YAML reflecting current AWS state             |
| `diff`               |     ✓     |      —      | Unified-diff per resource, current vs desired      |
| `apply` (`-dry-run`) |     ✓     |    `-dry-run` skips it    | Create / update                |
| `apply -prune`       |     ✓     |      ✓      | Also delete tracked resources missing from config  |
| `import-ecschedule`  |     —     |      —      | Convert an ecschedule YAML to ebschedule format    |

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

- A `trackingId:` is **required** in YAML; without it `-prune` is rejected.
- For **Rules**: only Rules whose tag matches the configured `trackingId`
  are eligible. Resources created by other tools (Terraform, CDK, console)
  remain untouched.
- For **Schedules**: ebschedule first checks the schedule **group** itself
  for the tracking tag. If absent, prune is skipped with a stderr warning
  — even if the config asks for an empty `schedules: []`. This protects
  groups shared between ebschedule and other tools.

A typical pattern:

```yaml
trackingId: my-app   # any stable string
groupName: my-app    # ebschedule-owned group
```

## Templating

Config files run through `text/template` **before** YAML parsing:

| Func                 | Notes                                                  |
| -------------------- | ------------------------------------------------------ |
| `{{ env "X" }}`      | Empty string if `X` is unset                           |
| `{{ must_env "X" }}` | Errors out (or placeholder under `validate`)           |
| `{{ ssm "/p/k" }}`   | SSM Parameter Store, decrypted, region from AWS creds  |

Under `validate`, AWS is never called: `ssm` returns `<ssm:/path>` and
`must_env` falls back to `<env:NAME>` so the structural check works
offline.

## Diff

Unified-diff (git-style) per resource, comparing remote vs desired YAML.

For Schedules, the comparison strips Scheduler's documented defaults
(`timezone: UTC`, `actionAfterCompletion: NONE`, `retryPolicy: {185, 86400}`)
on both sides, so a YAML that explicitly writes those defaults still
shows as no-op.

The internal `ebschedule-tracking-id` tag is hidden from diff.

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
  source: [aws.s3]
  detail-type: [Object Created]

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
- `containerOverrides` is encoded into the target's `input` JSON.
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
ebschedule -conf /dev/null \
  -tag Service=my-app \
  -tag Env=prod \
  dump > my-app.yaml

# Edit my-app.yaml as needed (most importantly: pick a trackingId), then
# round-trip with diff to confirm there's nothing to change:
$EDITOR my-app.yaml
ebschedule -conf my-app.yaml diff && echo "in sync"
```

Multiple `-tag` flags are AND-matched. Schedules dump per-group as
before; the `-tag` filter applies to Rules only.

### CI: gate PRs on drift

`diff` exits with code `2` when there's drift (terraform-plan style),
`0` when clean, `1` on error. A typical pipeline step:

```yaml
# .github/workflows/check-drift.yml
- run: ebschedule -conf 'config/*.yaml' diff
```

When `apply` is part of CI, pass `-auto-approve` (the interactive
prompt is suppressed automatically when stdin isn't a TTY, but being
explicit avoids surprises in shells that fake one):

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

`-target` and `-prune` are mutually exclusive (partial-target +
whole-config sweep would be incoherent). Naming a resource the config
doesn't define errors out rather than silently skipping.

### Splitting per-team or per-service

When several teams share an account, give each team its own config file
(and its own `trackingId`). Glob loads them all in one invocation but
keeps the prune scope per-file:

```sh
ebschedule -conf 'config/*.yaml' apply -prune
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

# ebschedule

A small declarative CLI for managing **Amazon EventBridge Rules** and
**EventBridge Scheduler Schedules** from a single YAML config. Inspired by
[Songmu/ecschedule](https://github.com/Songmu/ecschedule), generalized to
arbitrary targets and to both EventBridge services.

## Build

```sh
go mod tidy
go build -o ebschedule .
```

## Use

```sh
ebschedule -conf ebschedule.yaml validate                      # offline structural check
ebschedule -conf ebschedule.yaml dump my-app- > ebschedule.yaml
ebschedule -conf ebschedule.yaml diff
ebschedule -conf ebschedule.yaml apply -dry-run
ebschedule -conf ebschedule.yaml apply -prune

# Multiple files (e.g. one per service / team)
ebschedule -conf 'config/*.yaml' validate
ebschedule -conf 'config/*.yaml' apply -prune
```

The single CLI handles both Rules and Schedules. Whether each is processed
depends on whether the corresponding section appears in the YAML:

| YAML state         | Behavior                                                            |
| ------------------ | ------------------------------------------------------------------- |
| `rules:` omitted   | ebschedule does not touch Rules at all (incl. no prune)                 |
| `rules: []`        | ebschedule manages Rules; with `-prune`, deletes every tracked Rule     |
| `rules: [..items]` | ebschedule manages those items                                          |
| same for `schedules:` | same                                                            |

## Templating

Config files run through `text/template` before YAML parsing:

| Func                 | Notes                                              |
| -------------------- | -------------------------------------------------- |
| `{{ env "X" }}`      | Empty if `X` is unset                              |
| `{{ must_env "X" }}` | Errors if `X` is unset                             |
| `{{ ssm "/p/k" }}`   | SSM Parameter Store (decrypted, region from creds) |

## Tags

Top-level `tags:` applies to every rule and schedule. Per-resource `tags:`
override. On `apply`, ebschedule reconciles tags fully — adding missing ones and
removing tags present remotely that aren't in the desired set. The internal
`ebschedule-tracking-id` tag is always preserved when `trackingId` is set.

## Diff

Unified-diff (git-style) output per resource, comparing remote vs desired
YAML. The internal tracking tag is hidden from comparison.

## Prune safety

`-prune` deletes only resources carrying

```
ebschedule-tracking-id = <trackingId>
```

so it can never wipe rules / schedules created by other tools / stacks.
`trackingId` must be set in the YAML, otherwise `-prune` is rejected.

## Validate (offline)

`ebschedule validate` checks structure without hitting AWS:

- Required fields, name regex, max-length, unique names / target IDs
- Mutually-exclusive `scheduleExpression` vs `eventPattern`, valid `cron(...)`/`rate(...)`/`at(...)` form
- `eventPattern` and `target.input` are valid JSON
- Enum fields (`state`, `actionAfterCompletion`, `flexibleTimeWindow.mode`, `assignPublicIp`, `launchType`)
- Schedule `timezone` is a real IANA name
- `startDate` / `endDate` parse as RFC3339
- Tag keys/values within AWS limits and not reserved
- ECS target taskDefinitionArn presence

In validate mode, `ssm` is stubbed (`<ssm:/path>`) and `must_env` falls back to a
placeholder so it works fully offline. Exit code is non-zero if any problems
are found.

## Schedule groups

If `groupName:` refers to a group that doesn't exist yet, `apply` creates it
on the fly, propagating top-level `tags:` and the tracking tag. The `default`
group always exists and is skipped. Existing groups are left untouched —
ebschedule does not reconcile or delete schedule groups.

## Files

- `main.go` — CLI dispatch, unified `Config`, template/SSM helpers, tag reconciliation
- `rule.go` — EventBridge Rule operations
- `schedule.go` — EventBridge Scheduler operations (incl. group auto-create)
- `validate.go` — offline structural validation
- `ebschedule.yaml` — example with both rules and schedules

## Extend

To support more target shapes, add a field to the relevant `Target` /
`ScheduleTarget`, copy from remote in `fromRemote*`, and copy to AWS SDK type
in `toAWS*`. That's the entire pattern.

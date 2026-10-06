---
description: "Reference for `zordon status --format`: the text report, and the agent and json reports with only the fields an agent acts on."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/reference/status-formats/">https://zordon.io/reference/status-formats/</a></div>

# `zordon status --format`

`--format` selects how `zordon status` writes the stack.

| Format | Output |
| --- | --- |
| `text` | The report for people: every level of the federation chain, its imports and services. |
| `agent` | logfmt lines: the stack, then one line per service. |
| `json` | One JSON object on one line. |

`--format` is a global flag.
Left out, it is `agent` under `--agent` and `text` otherwise; an explicit `--format` wins over `--agent`.
The MCP server passes `--agent` to every command, so its `status` tool answers in the `agent` format unless called with `--format`.

The `agent` and `json` formats carry the same report and always exit 0: a stack that cannot be resolved is reported in `state` and `error`, so a caller parses stdout whatever happened.
Fields at their zero value are left out.

## agent

```text
workspace=feature state=running alphasfile=/proj/Alphasfile state_dir=/proj/workspaces/feature
service=api state=ready picked=true print="http://127.0.0.1:8080/  (api)"
service=postgres state=probing shared=true
```

The first line is the stack, each further line a service, with the fields of the tables below.
A value holding a space, a quote, an equals sign or a control character is quoted as a Go string.

## json

```json
{"workspace":"feature","alphasfile":"/proj/Alphasfile","state_dir":"/proj/workspaces/feature","state":"running","services":[{"name":"api","state":"ready","picked":true,"print":"http://127.0.0.1:8080/  (api)"},{"name":"postgres","state":"probing","shared":true}]}
```

## Stack

| Field | Type | Meaning |
| --- | --- | --- |
| `workspace` | string | The invocation's workspace; `main` at the project root. |
| `alphasfile` | string | Absolute path of the invocation's `Alphasfile`. |
| `state_dir` | string | The workspace's state directory, `<root>/workspaces/<workspace>`. |
| `state` | string | `running`, `stopped`, `error` or `no-alphasfile`. |
| `error` | string | Why the stack could not be resolved; only with `state: error`. |
| `services` | array | The services of every level of the federation chain, leaf and parents; in `agent`, one line each. |

`state` is `running` only while the invocation's own alpha answers; a running federation parent does not make it so.
`no-alphasfile` means no `Alphasfile` exists at or above the working directory: not a zordon project.

## Service

| Field | Type | Meaning |
| --- | --- | --- |
| `name` | string | The name `zordon status` and `zordon start` accept; `service` in `agent`. |
| `state` | string | `ready`, `unhealthy`, `probing`, `starting`, `failed` or `stopped`. |
| `health` | string | The probe error of an `unhealthy` service. |
| `picked` | bool | The workspace checks the service out as its own worktree (`workspace create <ws> <svc>`, `workspace service add`). |
| `shared` | bool | A federation parent runs the service, shared by every workspace. |
| `print` | string | The service's composed `print` line; only while its level's alpha runs, as a stopped level's ports were picked for the report alone. |
| `checkout` | string | Set only when an editable worktree is off its canonical branch, e.g. `on branch my-fix, not zordon/feature/api`. |
| `revision` | string | The short commit of a detached checkout. |
| `checkout_path` | string | The git tree the service builds from, absolute: its own repository, a workspace's worktree, or the directory of a package or module nested in either. |
| `branch` | string | The branch `checkout_path` is on; absent when detached. |
| `source_dir` | string | The service's own directory inside `checkout_path`, absolute (`fs::exe`); in a monorepo, services share `checkout_path` and differ here. |

`unhealthy` is a service alpha called ready whose readiness probe fails now; the probe runs once per `status`, with a 500 ms timeout.
The `text` format also lists each level's imports; the `agent` and `json` formats do not.

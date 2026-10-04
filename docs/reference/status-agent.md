---
description: "Reference for `zordon --agent status`: the stack as one JSON object, with only the fields an agent acts on."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/reference/status-agent/">https://zordon.io/reference/status-agent/</a></div>

# `zordon --agent status`

With `--agent`, `zordon status` prints one JSON object on one line instead of its text report.
The MCP server passes `--agent` to every command it runs, so its `status` tool answers with this object too.
It always exits 0: a failure to resolve the stack is reported in `state` and `error`, so a caller parses stdout whatever happened.
Fields at their zero value are left out.

```json
{"workspace":"feature","alphasfile":"/proj/Alphasfile","state_dir":"/proj/workspaces/feature","state":"running","services":[{"name":"api","state":"ready","picked":true,"print":"http://127.0.0.1:8080/"},{"name":"postgres","state":"probing","shared":true}]}
```

## Stack

| Field | Type | Meaning |
| --- | --- | --- |
| `workspace` | string | The invocation's workspace; `main` at the project root. |
| `alphasfile` | string | Absolute path of the invocation's `Alphasfile`. |
| `state_dir` | string | The workspace's state directory, `<root>/workspaces/<workspace>`. |
| `state` | string | `running`, `stopped`, `error` or `no-alphasfile`. |
| `error` | string | Why the stack could not be resolved; only with `state: error`. |
| `services` | array | The services of every level of the federation chain, leaf and parents. |

`state` is `running` only while the invocation's own alpha answers; a running federation parent does not make it so.
`no-alphasfile` means no `Alphasfile` exists at or above the working directory: not a zordon project.

## Service

| Field | Type | Meaning |
| --- | --- | --- |
| `name` | string | The service's name. |
| `state` | string | `ready`, `unhealthy`, `probing`, `starting`, `failed` or `stopped`. |
| `health` | string | The probe error of an `unhealthy` service. |
| `picked` | bool | The workspace checks the service out as its own worktree (`workspace create <ws> <svc>`, `workspace service add`). |
| `shared` | bool | A federation parent runs the service, shared by every workspace. |
| `print` | string | The service's composed `print` line. |
| `checkout` | string | Set only when an editable worktree is off its canonical branch, e.g. `on branch my-fix, not zordon/feature/api`. |

`unhealthy` is a service alpha called ready whose readiness probe fails now; the probe runs once per `status`, with a 500 ms timeout.

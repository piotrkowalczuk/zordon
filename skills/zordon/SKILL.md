---
name: zordon
description: Use in a repo that has an Alphasfile, or inside a zordon workspace copy, when a task touches the local dev stack — bringing services up or down, checking status or ports, or running a declared provision (migration, seeding, teardown). Prefer the zordon MCP tools over running the `zordon`/`alpha` binaries in Bash, and don't improvise a setup step that wasn't declared as a provision.
---

# Working with a zordon-managed stack

## When this applies

This applies when there is an `Alphasfile` at or above the working directory, or the working directory is a workspace copy — an isolated stack of its own.
A workspace directory carries a `.workspace` marker file (and lives at `workspaces/<name>/`); that marker is the unambiguous signal that you are inside a workspace.
Otherwise the project is not zordon-managed — ignore this skill and the zordon tools.

## Prefer a workspace

Default to an isolated workspace rather than the project's main/shared stack: a workspace is a disposable copy with its own ports and state, so your work never disturbs the developer's running stack.
Unless the user explicitly wants the main stack, ask — before `start`ing from the project root — whether to create and use a workspace instead.
Create one with `zordon workspace create <name>` and work from `workspaces/<name>/` (recognizable by its `.workspace` marker).
If the project declares a top-level `workspace { file ... }` block, `create` also writes those files (a `CLAUDE.md`, a dev container, hooks) into the new directory before checking anything out — so read them once you are in there.
They are generated at create time, never at start, so they can never carry a live port; use `get` or the MCP tools for that.
`zordon workspace apply --workspace=<name>` re-writes them after the Alphasfile changes.
Entering a workspace is by directory: the MCP tools act on the directory the server runs in, so `cd workspaces/<name>` and drive it with the `zordon` CLI there — a directory-independent `--workspace` selector for the MCP tools does not exist yet.

## Prefer the tools, not the shell

The zordon MCP tools are the declared interface to the stack; the `zordon`/`alpha` binaries in Bash are not. The tools return structured results, and an on-demand provision that fails reports an error but never tears the running stack down — so it is safe to poke a live stack through them.

## Task → tool

| Goal | Tool |
| --- | --- |
| Bring the stack up (or a subset — pass service names) | `start` |
| See what's running across the chain, and on which ports | `status` |
| Read one resolved value (a port, `ready`, the command) | `get <expr>`, e.g. `service.go.api.vars.port` |
| Preview the resolved config with no side effects | `plan` |
| Run a declared one-off (migrate, seed, create topic, teardown) | the matching `provision__<toolchain>_<service>__<step>` tool |
| Stand up an isolated parallel copy of the stack | the workspace tool — confirm its exact name via `tools/list` |
| Apply privileged wiring (DNS, port :80) | `sudo` |
| Stop this level | `stop` |
| Tear down a provision's side effects | `clean` |

For a command tool's flags and arguments, pass `["-h"]`; for a provision, read the tool's own description (it carries the resolved `cmd` and env keys).

## Ordering rules that trip agents up

- A provision runs *inside* a live supervisor. `start` before invoking a `provision__*` tool; a "no alpha running" error means you skipped `start`.
- `clean` refuses a running stack — `stop` first, or use `stop --clean` in one step. `stop` on its own never runs `clean`.
- `stop` and `clean` only affect your invocation level; shared parent infrastructure keeps running.
- Tool names may not match CLI subcommand names (the workspace tool in particular). Confirm names from `tools/list` rather than assuming.
- If the project uses a non-default `ZORDON_HOME`, the MCP server must run with the same value or it targets a different stack.

## Guardrail: don't improvise

If a task needs a step — reset the DB, seed fixtures — and no `provision__*` tool matches, that step wasn't declared for this project. Say so rather than reaching for raw SQL, ad-hoc scripts, or the filesystem.

## Deeper detail

- Lifecycle and states — <https://zordon.io/lifecycle/>
- Provisions — <https://zordon.io/reference/mcp/>, <https://zordon.io/how-to/run-a-provision-via-mcp/>
- Workspaces — <https://zordon.io/workspaces/>
- Federation and sudo — <https://zordon.io/federation/>

---
name: zordon
description: Use when a task touches this project's local dev stack — starting/stopping it, checking status, or running a declared provision (migrations, seeding, teardown) — in a repo that has an Alphasfile. Prefer the zordon MCP tools over running `zordon`/`alpha` in Bash.
---

# Working with a zordon-managed stack

If this project has an `Alphasfile`, its local stack (databases, brokers, services) is supervised by zordon, and the MCP tools below are the intended interface — not the `zordon` binary via Bash.

**Reach for the tools, not the shell:**
- `start` / `stop` — bring the declared stack up or down.
- `status` / `get` — check what's running and on which ports before assuming something is broken or re-starting it.
- `plan` — see the fully resolved config without side effects.
- `provision__<toolchain>_<service>__<step>` tools — the *only* sanctioned way to run one-off setup (migrations, seeding, fixture teardown). Each one maps to a step the project author explicitly declared; there is no general-purpose "run arbitrary command" tool.
- `sudo` / `workspace` — federation and isolated-copy operations; use them instead of hand-rolling directory tricks.

**Why not Bash:** the tool boundary is deliberate — it's the project's declared contract for what an agent may do to the stack, not an incidental convenience. Shelling out to `zordon` directly works but bypasses structured results and, for provisions, the guarantee that a failed provision never tears the running stack down.

**Before improvising:** if a task looks like "reset the DB" or "seed test data" and no provision tool matches, that step hasn't been declared for this project — say so rather than reaching for raw SQL or scripts.

No `Alphasfile` in this working directory → this skill and the zordon tools don't apply; ignore both.

---
name: issue-report
description: File a bug report or feature request against the zordon project (github.com/piotrkowalczuk/zordon) from a short description. Distills a generic, reproducible essence and strips anything specific to the user's local setup — absolute paths, usernames, hostnames, internal service/company names, concrete ports, secrets — before opening the issue. Invoke as /zordon:issue-report <description>.
argument-hint: <description>
disable-model-invocation: true
allowed-tools:
  - Bash(gh auth status)
  - Bash(gh issue list *)
  - Bash(zordon --version)
  - Bash(zordon -h)
  - Bash(uname *)
---

# Report a zordon issue upstream

Open a bug report or feature request against **github.com/piotrkowalczuk/zordon** from the user's `<description>`, after reducing it to a generic, reproducible essence and stripping anything specific to their local setup.

## Steps

1. **Classify** the report from the description and the conversation: bug, feature request, or question.
2. **Collect safe environment only** — zordon version (`zordon --version`, else the `zordon -h` header) and OS/arch (`uname -sm`). Never collect paths, env values, or secrets.
3. **Draft** a sanitized title and body (templates below), reducing the user's situation to the smallest generic reproduction expressed in zordon terms.
4. **Check for duplicates**: `gh issue list --repo piotrkowalczuk/zordon --search "<keywords>" --state all`. If a close match exists, show it and ask whether to comment there instead of opening a new one.
5. **Confirm before filing.** Show the user the full drafted issue (title + body) and get explicit approval; let them edit it. Never run `gh issue create` without a clear yes — this opens a public issue on a third-party repository.
6. **File it**: `gh issue create --repo piotrkowalczuk/zordon --title "<title>" --body "<body>"`. If `gh` is not authenticated (`gh auth status` fails), tell the user to run `gh auth login` first and stop.
7. Return the created issue URL.

## Sanitize — strip local details, keep the generic essence

This is the point of the skill: the upstream maintainer needs the *shape* of the problem, not the user's environment.

Remove and replace with neutral placeholders:
- Absolute paths and home dirs (`/Users/<name>/…`, `/home/<name>/…`) → `<path>`.
- Usernames, hostnames, IPs (except loopback), emails, and org / company / product / internal service names → `service-a`, `app`, `<host>`.
- Concrete dynamic ports, content hashes, temp dirs, and any environment-variable *values* → placeholders.
- Secrets, tokens, and connection strings → never include, under any circumstances.
- The user's own repository URLs and git remotes.

Keep (these are generic and genuinely useful):
- zordon subcommands, flags, toolchains (`go` / `rust` / `ruby` / `nodejs` / `pkg`), and config helpers (`net::pickport()`, `fs::tmp()`, `fs::etc()`, …).
- Error text, **with paths redacted**.
- zordon version and OS/arch.

Rewrite any reproduction `Alphasfile` with generic service names and only the helpers/fields actually involved — not the user's real manifest.

## Bug template

```
### Environment
- zordon: <version>
- OS/arch: <os>/<arch>

### What happens
<one generic paragraph>

### Expected
<what should happen instead>

### Minimal repro
<sanitized Alphasfile snippet + the command run>

### Logs
<relevant lines, paths redacted>
```

## Feature template

```
### Problem
<the generic need, not the user's specific project>

### Proposed behavior
<what zordon could do>

### Alternatives considered
<optional>
```

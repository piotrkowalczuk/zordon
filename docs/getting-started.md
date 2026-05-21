# Getting started

## Install

```sh
go install github.com/piotrkowalczuk/zordon/cmd/...@latest
```

This installs both `zordon` and `alpha` into your `$GOBIN` (or
`$GOPATH/bin`). Make sure that directory is on your `$PATH`.

## A first stack

Create an `Alphasfile` in your project root — start from the
[simple example](https://github.com/piotrkowalczuk/zordon/blob/master/examples/simple/Alphasfile)
— then:

```sh
zordon start              # resolve, build, bring the stack up, stream logs
zordon status             # what's running across the whole chain right now?
zordon stop               # shut this level's services down
zordon worktree create x  # a parallel, isolated copy of the whole stack
zordon get <expr>         # print one resolved value (path or Go template)
```

`zordon start` returns as soon as every service is READY (so your shell
is free); `alpha` keeps the stack running in the background until
`zordon stop` or a kill.

## Useful flags

```
zordon --agent              machine-friendly log format ('<ms> <src> <LEVEL> <msg>')
zordon start --failfast     abort bringup and shut down on first failure (default on)
zordon start --alpha-log P  where alpha appends its own log
zordon start --timeout 60s  max wait for alpha to come up & finish bringup
```

## Next

- [Concepts](concepts.md) — the handful of ideas the rest builds on
- [Alphasfile](alphasfile.md) — every field, with examples
- [Services](services/go.md) — per-toolchain build/run: Go, Rust, Ruby, Node.js

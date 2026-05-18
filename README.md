# Zordon

Zordon runs the supporting stack for local agentic workflows — the
databases, queues, brokers, and toy services an agent needs to actually
exercise your code while it works.

The constraint is resourcefulness. An agent loop spins these dependencies
up and tears them down constantly; every second of cold start and every
megabyte of idle overhead is paid back many times a day. Containers solve
isolation by paying that cost upfront — fine for production, wasteful here.
Zordon runs the same services as plain host processes, supervised
together, declared once in an `Alphasfile`.

## Documentation

Full docs: **<https://piotrkowalczuk.github.io/zordon/>**
(source in [`docs/`](docs/), built with MkDocs Material).

- [Alphasfile](docs/alphasfile.md) — the manifest: services, source pointers, readiness, logs
- [Dynamic configuration](docs/dynamic-config.md) — the DAG, helpers, `self`, cross-service refs
- [Worktrees](docs/worktrees.md) — parallel, isolated copies of the whole stack
- [Federation](docs/federation.md) — chained Alphasfiles, shared infra, `zordon sudo`

## Installation

```sh
go install github.com/piotrkowalczuk/zordon/cmd/...@latest
```

Installs `zordon` and `alpha` into your `$GOBIN` (or `$GOPATH/bin`) —
make sure that directory is on your `$PATH`.

## Quick start

Create an `Alphasfile` (see [examples/simple](examples/simple/Alphasfile)), then:

```sh
zordon start              # spawn alpha, push config, stream bringup logs
zordon status             # what's running across the whole chain right now?
zordon stop               # ask alpha to shut its children down and exit
zordon worktree create x  # a parallel, isolated copy of the stack
```

See the [docs](https://piotrkowalczuk.github.io/zordon/) for everything else.

## License

[GNU General Public License v3.0](https://www.gnu.org/licenses/gpl.txt),
see [LICENSE.md](LICENSE.md).

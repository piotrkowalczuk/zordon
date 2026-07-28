---
title: "Define a Java service built by its wrapper"
description: "Java services clone a repository, build through its committed Maven or Gradle wrapper, and run the resulting jar under a mise-installed JDK."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/services/java/">https://zordon.io/services/java/</a></div>

# Define a Java service built by its wrapper

```hcl
toolchain { java { version = "temurin-21.0.5+11.0.LTS" } }

service "java" "petclinic" {
  git { url = "github.com/spring-projects/spring-petclinic" }

  vars = { port = net::pickport() }
  env  = { SERVER_PORT = "${self.vars.port}" }

  readiness { http { path = "/" port = self.vars.port } }
}
```

The block label is `java`, matching the toolchain sub-block (1:1).
The mise tool is also `java`; mise's java plugin exports `JAVA_HOME`, which the build tool and the runtime both use.

## Source

`git` (zordon bare-clones) or `src` (your local checkout).
`branch` / `tag` / `rev` pin the revision.
Relative `src` resolves against the Alphasfile's directory.
There is **no use-only mode** (no `package` / `crate`) — Java services are app-style, fetched from a repo or a local dir.

## `exe` — project root inside the checkout

`exe` is the path within the checkout that holds `pom.xml` / `build.gradle`, default `.`.
Set it for monorepo subdirs (`exe = "apps/api"`).
Build and runtime both `cd` into `<checkout>/<exe>`.

## Build (default)

The default build is **wrapper-first**: zordon runs the project's committed Maven or Gradle wrapper, so the exact build-tool version the project pins is what runs (only the JDK comes from mise).

| files at the exe-anchor        | build command                                   |
|--------------------------------|-------------------------------------------------|
| `pom.xml` + `mvnw`             | `./mvnw -q -B -DskipTests package`              |
| `build.gradle[.kts]` + `gradlew` | `./gradlew --console=plain --no-daemon -x test build` |
| a build file but no wrapper    | hard error (add a wrapper, or declare `build { cmd }`) |

A wrapper is required for the default build because the JDK ships no build tool.
Generate one with `mvn -N wrapper:wrapper` or `gradle wrapper`, or declare the build yourself:

```hcl
build { cmd = ["./mvnw", "-q", "-Pnative", "package"] }
```

!!! note "Git-introspecting build plugins"
    A `git` source is checked out into a **linked git worktree** (its `.git` is a file, not a directory).
    Plugins that read git metadata via JGit — e.g. `git-commit-id-maven-plugin`, used by Spring PetClinic — may fail with "Could not get HEAD Ref" there.
    Skip the plugin (`build { cmd = [..., "-Dmaven.gitcommitid.skip=true", ...] }`) or use a `src` local checkout, which is an ordinary repo.
    The [`examples/java`](https://github.com/piotrkowalczuk/zordon/tree/main/examples/java) PetClinic manifest shows the skip.

## Run (default)

Java has no single-binary artifact, so the runtime is **inferred from the jar the build produced** — the Java analog of Node reading `package.json`:

- Maven → the single `target/*.jar`
- Gradle → the single `build/libs/*.jar`

Non-runnable siblings are excluded: `*-sources.jar`, `*-javadoc.jar`, Gradle's `*-plain.jar`, and Maven's Spring-Boot `*.jar.original`.
The result is run as `java -jar <jar>`.
Zero or multiple candidates is a hard error asking you to declare `runtime { cmd = [...] }`.

The inferred run passes no argv, so feed configuration via env (idiomatic for Spring Boot's relaxed binding), e.g. `env = { SERVER_PORT = "${self.vars.port}" }`.
For explicit argv use `runtime { cmd = ["java", "-jar", "target/app.jar", "--server.port=${self.vars.port}"] }`.

## Version pin

`toolchain { java { version = "temurin-21.0.5+11.0.LTS" } }` is required (no fallback to a host JDK).
The value is any ref mise's `java` plugin accepts (`21`, `temurin-21`, a full LTS string); run `mise ls-remote java` to list them.
The JDK sits under `~/.zordon/toolchain/installs/java/<version>`; the user's host JDK stays independent.

There is no `toolchain.java.tools` — the JDK has no per-toolchain tool world; Maven/Gradle come from the project wrapper.

## TTY

Java services do **not** get a PTY by default: the JVM's `System.out` and logback's `ConsoleAppender` flush per line under a pipe, so startup logs flow without one.
Force a PTY with `log { tty = true }` if a runtime block-buffers its stdout.

# Filter noisy service logs

A talkative service can bury the output you care about.
A per-service `log { filter = "<expression>" }` block drops matching lines at the source — before they reach the streamed output or alpha's log file.

The expression is a boolean predicate over each line; when it is true the line is **dropped**.
Write it in an HCL heredoc (`<<-EOT … EOT`) so the expression's own quotes and backslashes reach the filter untouched.

Recipes below are copy-paste starting points; the [log filter reference](../reference/log-filter.md) is the full grammar.

!!! tip "Order does not matter for speed"
    The compiler runs the cheapest predicate first regardless of how you write the expression, so put the clause that reads best first and let the planner handle performance.

## Drop stack-trace / backtrace frames

Ruby, Python, and Java stack traces are indented continuation lines.
Match them with a raw-byte prefix test — the cheapest predicate there is.

```hcl
service "ruby" "api" {
  log {
    filter = <<-EOT
      hasPrefix(line, "\tfrom ") or hasPrefix(line, "/Users/")
    EOT
  }
}
```

## Drop debug/verbose logs, keep the rest

Match on the level, but gate the structured read behind a cheap substring so plain lines never hit the parser.

```hcl
log {
  filter = <<-EOT
    contains(line, "level") and severity(line) <= DEBUG
  EOT
}
```

`severity(line)` reads JSON `level`, logfmt `level=`, or a bare token and maps it to `TRACE(0) < DEBUG(1) < INFO(2) < WARN(3) < ERROR(4) < FATAL(5)`.
A line with no recognizable level is never dropped by a `severity(...)` test.

## Keep only warnings and errors

An allow-list is a deny-list negated: drop everything that is *not* what you want.

```hcl
log {
  filter = <<-EOT
    contains(line, "level") and not (severity(line) >= WARN)
  EOT
}
```

## Silence a specific noisy endpoint

Reach a nested JSON field with a gjson path, gated by a cheap substring so only the relevant lines are parsed.

```hcl
log {
  filter = <<-EOT
    contains(line, "/healthz") and json(line, "http.status") == 200
  EOT
}
```

`json(line, "path")` supports nested keys (`http.status`), array indexes (`tags.0`), and numeric comparisons (`>= 500`).

## Filter logfmt logs by field

```hcl
log {
  filter = <<-EOT
    contains(line, "component=") and logfmt(line, "component") == "gossip"
  EOT
}
```

## Drop noise from stderr only

`stream` is `"stdout"` or `"stderr"` and needs no parsing, so it is free to test.

```hcl
log {
  filter = <<-EOT
    stream == "stderr" and matches(line, "^W[0-9]")
  EOT
}
```

Prefer `contains`/`hasPrefix` over `matches`: a regex is by far the most expensive predicate.

## Verify a filter

Bring the stack up and watch the output, or resolve the config without running it:

```sh
zordon plan   # shows the resolved filter for each service
zordon start  # dropped lines never appear
```

A malformed expression is a configuration error: `zordon plan` and `zordon start` report it with the reason and abort before anything runs.

For the complete list of variables, functions, operators, and cost weights, see the [log filter reference](../reference/log-filter.md).

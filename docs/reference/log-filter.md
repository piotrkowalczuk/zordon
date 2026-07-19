# Log filter DSL

A per-service `log { filter = "<expression>" }` drops log lines at the source.
The expression is a boolean predicate evaluated against each raw line; when it is true the line is dropped — it reaches neither the streamed output nor alpha's log file.
An empty or absent `filter` keeps every line.

The filter runs inside alpha, before the line is written or forwarded, so a dropped line costs only the predicate: no string allocation, no JSON marshal, no socket write.

## Placement

```hcl
service "go" "app" {
  log {
    filter = <<-EOT
      hasPrefix(line, "\tfrom ") or (contains(line, "level") and severity(line) <= DEBUG)
    EOT
  }
}
```

Use an HCL heredoc (`<<-EOT … EOT`) so the expression's own `"` and `\` are not fought over by HCL string escaping.

## Variables

| Name | Type | Source |
| --- | --- | --- |
| `line` | string | the raw log line, without the trailing newline |
| `stream` | string | `"stdout"` or `"stderr"` |
| `service` | string | the emitting service's name |

`line` is only valid as the first argument to a function; `stream` and `service` are compared directly.

## Functions

| Call | Meaning | Cost |
| --- | --- | --- |
| `contains(line, "s")` | `s` occurs anywhere in the line | cheap |
| `hasPrefix(line, "s")` | line begins with `s` | cheap |
| `hasSuffix(line, "s")` | line ends with `s` | cheap |
| `matches(line, "re")` | RE2 regex matches the line | expensive |
| `json(line, "path")` | value at a gjson path (e.g. `http.status`, `tags.0`) | lazy |
| `logfmt(line, "key")` | value of `key=` in a logfmt line | lazy |
| `severity(line)` | the line's level as an ordinal (see below) | lazy |

`json`, `logfmt`, and `severity` extract only the referenced field; they never parse the whole line.

## Operators

Boolean: `and`, `or`, `not`, and parentheses.
Comparison: `==`, `!=`, `<`, `<=`, `>`, `>=`.

`stream`, `service`, `logfmt(...)`, and a `json(...)` compared to a string accept only `==` and `!=`.
`severity(...)` and a `json(...)` compared to a number accept all six.
A literal may appear on either side of a comparison.

## Severity

`severity(line)` reads a level from the line — JSON `level`/`severity`/`lvl`, logfmt `level=`, or a bare token — and maps it to an ordinal:

```
TRACE(0) < DEBUG(1) < INFO(2) < WARN(3) < ERROR(4) < FATAL(5)
```

Syslog aliases (`err`, `crit`, `emerg`, …) are recognized.
The uppercase names above are usable as constants, e.g. `severity(line) < WARN`.
A line with no recognizable level is treated as unknown and is never dropped by a `severity(...)` comparison.

## Evaluation order and cost

The compiler reorders each `and`/`or` so the cheapest predicate runs first, regardless of how the expression is written; short-circuiting then means an expensive term is reached only for the lines a cheaper term did not already decide.
So `severity(line) < WARN and stream == "stdout"` runs the free `stream` check first and scores severity only on `stdout` lines.
Predicates are pure and total, which is what makes this reordering safe.
Prefer `contains`/`hasPrefix` over `matches`: a regex is by far the most expensive predicate.

## Semantics

The predicate selects lines to **drop**.
Express an allow-list by negation: `not (severity(line) >= WARN)` keeps only warnings and above.

A malformed expression is a configuration error: it is reported by `zordon plan`/`start` before the stack comes up, never at runtime.

## Examples

```hcl
# drop ruby backtrace frames (cheap, raw-byte)
filter = hasPrefix(line, "\tfrom ") or hasPrefix(line, "/Users/")

# drop debug JSON logs, gated so plain lines never hit the parser
filter = contains(line, "level") and severity(line) <= DEBUG

# drop successful health checks, reaching a nested field lazily
filter = contains(line, "/healthz") and json(line, "http.status") == 200

# keep only errors on stderr
filter = stream == "stderr" and not (json(line, "level") == "error")
```

See `internal/logfilter` for the implementation and benchmarks.

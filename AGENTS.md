# Project Requirements and Constraints

## Code
- Be conservative about the amount of comments. Reseve them for outliers hat are not obvious or counterintuitive.
- Place private identifiers bellow public ones.
- **Errors vs. Panics (API Contracts):**
  - Use `error` exclusively for operational failures, environmental issues, or invalid external/user input (e.g., network timeouts, file missing, malformed request). These are states the program must gracefully handle.
  - Use `panic` for **programmer errors and API misuse**. If a developer violates an API contract—such as an out-of-bounds index in a data structure, passing a `nil` pointer where a value is strictly required, or calling methods in an invalid state/sequence—the code MUST `panic`.
  - Do not return an `error` to gently handle a bug in the code. A `panic` correctly signals that the code itself is fundamentally broken and must be fixed by the programmer.
- 12 factor app - https://12factor.net/

## Testing

- **Deliberate use of `t.Parallel()`:**
  - Do not blindly apply `t.Parallel()` to every test function.
  - **When to use:** Use it for I/O-bound tests, long-polling, simulated network delays, or moderate blocking operations that would otherwise bottleneck the test suite without consuming excessive hardware resources.
  - **When NOT to use (Resource Constraints):** NEVER use `t.Parallel()` if the test is heavily resource-intensive (e.g., allocates massive amounts of memory, monopolizes CPU cores, or requires a large number of DB connections). Running such tests in parallel can exhaust the host's resources (especially on CI runners), leading to OOM kills, timeouts, or connection pool exhaustion.
  - **When NOT to use (State Mutation):** Strictly FORBIDDEN if the test mutates global state, modifies environment variables (`os.Setenv`), or interacts with a shared, unisolated mutable resource.
  - **When NOT to use (Rate Limiting & Throttling):** NEVER use `t.Parallel()` if the test interacts with external APIs, third-party services, or internal systems that enforce strict rate limits. Concurrent execution will easily trigger rate-limit violations (e.g., `HTTP 429`) and cause flaky test failures.
- Use test names like
  - `Test<TypeName>_<MethodName>_<optionalAdditionalContext>
  - `Test<TypeName>_<MethodName>
  - `Test<TypeName>
  - `Test<FunctionName>_<optionalAdditionalContext>
  - `Test<FunctionName>

### Parametrized Tests
```go
func TestExample(t *testing.T) {
    cases := map[string]struct{...}{...}

    for hint, c := range cases {
        t.Run(hint, func(t*testing.T) {
            // ...
        }
    }
}
```

- Do NOT abuse the test case struct: The struct defining your test cases should contain purely data (inputs, expected outputs, expected errors).
- The Closure Anti-Pattern (STRICTLY FORBIDDEN): Do not add closures, anonymous functions, or complex setup/teardown hooks (e.g., setupMock func(), assertBehavior func()) into the test struct just to force different scenarios into a single table. This makes the test unreadable, impossible to debug, and ruins stack traces.
- When to split tests: If different test cases require drastically different setup logic, varied mock behaviors, or complex state initialization, stop using a table. Instead, write separate TestXxx functions.
- Shared Helpers over Fat Structs: To avoid code duplication across these separate tests, extract the common logic into shared helper functions (e.g., setupTestDB(t *testing.T), assertUser(t, expected, actual)). Keep the test flow straightforward and imperative.

## Workflow
- each new feature needs to have it's own isolated example

## Security & "Golden Path" Abstractions

- **Use Internal Abstractions (The Golden Path):** NEVER use the standard `os` package (e.g., `os.Open`, `os.ReadFile`, `os.Remove`) or `os/exec` directly within application or business logic. You MUST use the project's internal abstractions (e.g., `internal/fs`, `internal/sys`, or equivalent provided packages). These wrappers enforce strict security policies, path sanitization, and auditing.
- **Path Traversal Prevention:** Never blindly concatenate file paths, especially those containing user input. If you must manipulate paths (only within allowed system packages), always use `filepath.Clean` and verify the target path strictly resides within the expected base directory.
- **No Default HTTP Clients:** NEVER use `http.Get`, `http.Post`, or the default `http.Client`. They lack timeouts and are vulnerable to resource exhaustion (e.g., Slowloris) and SSRF. Always use the project's internal HTTP client wrapper. If forced to instantiate one, you MUST set strict `Timeout` values.
- **Randomness & Secrets:** NEVER use `math/rand` for generating session IDs, tokens, passwords, or any security-sensitive data. You MUST use `crypto/rand`.
- **SQL Injection Prevention:** NEVER construct SQL queries using string formatting, concatenation (`+`), or `fmt.Sprintf`. Always use parameterized queries (e.g., `?` or `$1`) through the project's database layer.
- **Environment Variables:** Do not read environment variables directly via `os.Getenv` deep inside business logic. All configuration must be loaded, validated, and sanitized at startup (in `cmd/` or configuration init), and passed down via typed structs.

## Documentation Guidelines (Diátaxis Framework)

When creating, updating, or managing documentation in the `./docs` directory, you **MUST** strictly follow the [Diátaxis framework](https://diataxis.fr/).

Documentation is not a single monolith. Every document you write must fit into **exactly one** of the four quadrants below. **Do not mix these types in a single file.**

Before writing any documentation, determine the user's current need and place the file in the appropriate subdirectory.

### 1. Tutorials (`./docs/tutorials/`)
**Goal:** Learning-oriented (Allow the newcomer to get started).
* **Focus:** Guide the user by the hand through a comprehensive project setup or basic usage.
* **Tone:** Instructive, encouraging, step-by-step.
* **Agent Rules:**
  * Focus on a successful first experience.
  * Do NOT include abstract explanations or exhaustive reference details.
  * If a step requires deep knowledge, skip the theory and just tell the user what to type.

### 2. How-To Guides / Cookbook (`./docs/how-to/`)
**Goal:** Problem-oriented (Help the user achieve a specific task).
* **Focus:** Practical, step-by-step recipes for users who already know the basics.
* **Tone:** Direct, action-oriented (e.g., "How to configure the database", "How to create a custom user").
* **Agent Rules:**
  * Assume the user has basic knowledge of the system.
  * Focus purely on the steps required to solve the specific problem.
  * Omit unnecessary background explanations.

### 3. Reference (`./docs/reference/`)
**Goal:** Information-oriented (Provide comprehensive, accurate facts).
* **Focus:** Code behavior, API endpoints, function signatures, configurations, and data structures.
* **Tone:** Dry, objective, to the point.
* **Agent Rules:**
  * Describe *what* the machinery does, what inputs it takes, and what outputs it returns.
  * Do NOT include step-by-step usage tutorials or conceptual explanations here.
  * Keep it structured, highly organized, and easy to scan.

### 4. Explanation (`./docs/explanation/`)
**Goal:** Understanding-oriented (Explain the context and architecture).
* **Focus:** High-level concepts, architectural decisions, "Why" things are designed the way they are.
* **Tone:** Discursive, analytical, informative.
* **Agent Rules:**
  * Do NOT include how-to steps or code references here.
  * Focus on the bigger picture, historical context, or design alternatives that were considered.

---

### 🛑 Strict Agent Directives:
1. **Never mix quadrants:** Do not put a tutorial inside a reference document. If a reference needs an example, keep it minimal. If it needs a guide, link to a How-To guide.
2. **Directory mapping:** Always save your markdown files in the corresponding `./docs/` subdirectories:
   * `./docs/tutorials/`
   * `./docs/how-to/`
   * `./docs/reference/`
   * `./docs/explanation/`
3. **Cross-linking:** Actively use Markdown links to connect related concepts across the four quadrants instead of duplicating information.

### Markdown
- One sentence, one row. Do not wrap lines.

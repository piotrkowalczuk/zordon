# How to run a provision via MCP

This recipe runs a single provision on demand from an MCP client, without bringing the rest of the stack up or down.
It assumes you already know the basics of [Alphasfile](../alphasfile.md) and provisions; for the full tool reference see [`zordon mcp`](../reference/mcp.md).

## 1. Expose a provision

Declare the provision in your `Alphasfile`.
A latent provision (`after = never`) never auto-runs at bringup — it exists purely to be invoked on demand:

```hcl
service "go" "app" {
  runtime {
    provision "seed-data" {
      after = never
      argument "key" {
        type     = "string"
        required = true
      }
      cmd = "echo ${self.runtime.provision.seed-data.arguments.key} > ${self.vars.seed}"
    }
  }
}
```

Declared `argument` blocks become typed fields on the MCP tool, so the agent sees exactly what to pass; see the [reference](../reference/mcp.md#typed-arguments) for the full rules.
Any provision can be invoked over MCP, latent or not; latent ones are the natural fit for "run like a script".

## 2. Start alpha

Provisions run inside the live alpha, so bring the stack up first:

```sh
zordon start
```

## 3. Point your MCP client at `zordon mcp`

Configure your client to launch the server as a subprocess from the project directory.
For a client that reads a JSON server config:

```json
{
  "mcpServers": {
    "zordon": {
      "command": "zordon",
      "args": ["mcp"],
      "cwd": "/path/to/your/project"
    }
  }
}
```

Set the same `ZORDON_HOME` the project uses if it is not the default, so the server resolves the same alpha.

## 4. Discover the provision tool

List the tools and find the one named `provision__<toolchain>_<service>__<step>`.
For the service above that is `provision__go_app__seed-data`.

## 5. Invoke it

Call the tool, passing its declared arguments (and, if needed, undeclared overrides via `env`):

```json
{"name": "provision__go_app__seed-data", "arguments": {"key": "abc"}}
```

The tool result contains the provision's shell output.
If it fails, the result is marked as an error — but alpha keeps running (see the [invariant](../reference/mcp.md#invariant-a-failed-invoke-never-kills-alpha)).

## 6. Verify

The provision ran inside alpha, so its side effects are real: the file written above now contains `abc`.
Calling `status` (the command tool, or `zordon status` on the CLI) confirms the stack is still up.

## See also

- A complete, runnable version of this recipe lives in `examples/mcp/` (driven end-to-end by `examples/mcp/example_test.go`).
- [`zordon mcp` reference](../reference/mcp.md) for tool names, input shapes, and the `OpInvoke` wire format.

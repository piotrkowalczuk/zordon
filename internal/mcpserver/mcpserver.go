// Package mcpserver holds the SDK-agnostic logic behind `zordon mcp`: turning
// the resolved service set into provision tool descriptors and minting valid,
// unique MCP tool names. The go-sdk wiring (server, transport, handlers) lives
// in cmd/zordon so it can reach the command tree and the federation chain; this
// package stays pure so the mapping can be unit-tested without a live alpha.
package mcpserver

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// maxCmdPreview caps the resolved cmd snippet embedded in a tool description —
// enough to recognize the action, not so long it bloats the tool list.
const maxCmdPreview = 200

// Provision is a flattened, tool-ready view of one provision step. ID is the
// canonical identity alpha keys provisions by and the value zordon sends in an
// OpInvoke request.
type Provision struct {
	ID        string // service.<tc>.<svc>.runtime.provision.<name>
	Toolchain string
	Service   string
	Step      string
	Latent    bool
	Detached  bool
	Cmd       string
	Doc       string                     // author's `description` from the Alphasfile, if any
	Args      []*alphasfile.ProvisionArg // declared typed inputs (MCP tool fields)
	EnvKeys   []string
}

// Provisions flattens every provision across the given services, in service
// then declaration order.
func Provisions(services []*alphasfile.Service) []Provision {
	var out []Provision
	for _, s := range services {
		if s == nil || s.Runtime == nil {
			continue
		}
		for _, p := range s.Runtime.Provision {
			if p == nil {
				continue
			}
			out = append(out, Provision{
				ID:        ProvisionID(s.Toolchain, s.Name(), p.Name),
				Toolchain: s.Toolchain,
				Service:   s.Name(),
				Step:      p.Name,
				Latent:    p.Latent,
				Detached:  p.Detached,
				Cmd:       p.Cmd,
				Doc:       p.Description,
				Args:      p.Arguments,
				EnvKeys:   sortedKeys(p.Env),
			})
		}
	}
	return out
}

// ProvisionID builds the canonical provision identity. It MUST match alpha's
// newProvisionCtx id ("service."+toolchain+"."+name + ".runtime.provision."+
// step) or OpInvoke will not resolve the target.
func ProvisionID(toolchain, service, step string) string {
	return "service." + toolchain + "." + service + ".runtime.provision." + step
}

// ToolName is the (unsanitized) base MCP tool name for this provision. Pass it
// through Unique to guarantee global uniqueness across a chain.
func (p Provision) ToolName() string {
	return SanitizeToolName("provision__" + p.Toolchain + "_" + p.Service + "__" + p.Step)
}

// Description is the tool description shown to the agent. It surfaces what the
// provision does (resolved cmd), its latent/detached nature, the env keys it
// reads, and the invariant that running it never kills alpha.
func (p Provision) Description() string {
	var b strings.Builder
	// Author's own words lead, when given; the synthesized facts follow so the
	// agent still sees the cmd, env keys, and the no-kill-alpha invariant.
	if doc := strings.TrimSpace(p.Doc); doc != "" {
		b.WriteString(doc)
		if !strings.HasSuffix(doc, ".") {
			b.WriteByte('.')
		}
		b.WriteByte(' ')
	}
	fmt.Fprintf(&b, "Run provision %q on service %s.%s", p.Step, p.Toolchain, p.Service)
	var tags []string
	if p.Latent {
		tags = append(tags, "latent: never auto-runs, exposed for on-demand invocation")
	}
	if p.Detached {
		tags = append(tags, "detached")
	}
	if len(tags) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(tags, "; "))
	}
	b.WriteByte('.')
	if c := strings.TrimSpace(p.Cmd); c != "" {
		fmt.Fprintf(&b, " cmd: %s", truncate(c, maxCmdPreview))
	}
	if len(p.EnvKeys) > 0 {
		fmt.Fprintf(&b, " Reads env: %s.", strings.Join(p.EnvKeys, ", "))
	}
	b.WriteString(" Runs inside the live alpha; a failure is reported but never shuts alpha down.")
	return b.String()
}

// InputSchema is the provision tool's JSON-schema input: one typed property per
// declared argument (with description/default, required listed) plus a generic
// `env` object as an escape hatch for undeclared environment overrides. Built
// as a plain map so this package stays free of any MCP-SDK dependency; it
// JSON-marshals to a valid 2020-12 object schema.
func (p Provision) InputSchema() map[string]any {
	props := map[string]any{}
	var required []string
	for _, a := range p.Args {
		if a == nil {
			continue
		}
		prop := map[string]any{"type": jsonType(a.Type)}
		if a.Description != "" {
			prop["description"] = a.Description
		}
		if a.Default != nil {
			prop["default"] = a.Default
		}
		props[a.Name] = prop
		if a.Required {
			required = append(required, a.Name)
		}
	}
	props["env"] = map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string"},
		"description":          "extra environment variables for the provision shell",
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// jsonType maps a ProvisionArg type to its JSON-schema type.
func jsonType(t string) string {
	switch t {
	case "number":
		return "number"
	case "bool":
		return "boolean"
	default:
		return "string"
	}
}

var invalidToolChar = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// SanitizeToolName maps an arbitrary string onto the MCP tool-name grammar
// (^[A-Za-z0-9_-]{1,64}$): runs of invalid characters collapse to a single
// underscore and the result is clamped to 64 bytes. Empty input yields "_".
func SanitizeToolName(s string) string {
	s = invalidToolChar.ReplaceAllString(s, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return "_"
	}
	return s
}

// Unique returns base if unseen, else base with a numeric suffix, and records
// the chosen name in seen. It keeps tool names distinct when two federation
// levels expose a provision that sanitizes to the same base.
func Unique(seen map[string]struct{}, base string) string {
	if _, ok := seen[base]; !ok {
		seen[base] = struct{}{}
		return base
	}
	for i := 2; ; i++ {
		candidate := SanitizeToolName(fmt.Sprintf("%s_%d", base, i))
		if _, ok := seen[candidate]; !ok {
			seen[candidate] = struct{}{}
			return candidate
		}
	}
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

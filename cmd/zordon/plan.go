package main

import (
	"context"
	"fmt"
	"io"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/probe"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zdoc"
)

// runPlan resolves every Alphasfile in the federation chain statically
// (no alpha, no spawn) and renders each one back to HCL with every
// interpolation already substituted by its concrete value. Any
// unresolvable expression — `net::pickport()` that can't bind, an
// unknown `service.X.Y` ref, a cycle in the DAG, a missing env var —
// is a fatal error: plan is the "every value bakes down to a constant
// before we touch the system" preflight, not a partial dump.
//
// picks subsets the invocation (leaf) level exactly as `zordon start`
// does — the named services plus their transitive `after` deps — so a
// preflight of "just the subset I'm about to start" renders the same
// service set start would bring up. Parents are always rendered whole
// (they are shared context, not what you're starting).
func runPlan(_ context.Context, w io.Writer, zordonHome string, picks []string, testCfg alphasfile.TestConfig) error {
	levels, err := walkChain(zordonHome, func(lv *level) (*protocol.StateInfo, error) {
		af, err := alphasfile.Open(lv.afPath, lv.inv, lv.parentCtx, lv.cfgHash, testCfg)
		if err != nil {
			return nil, err
		}
		if err := validateInPlaceSources(af); err != nil {
			return nil, err
		}
		st := stateFromAlphasfile(af)
		if lv.isInvocation && len(picks) > 0 {
			filtered, err := pickServices(af.All(), picks)
			if err != nil {
				return nil, err
			}
			st.Services = filtered
		}
		return st, nil
	})
	if err != nil {
		return err
	}
	for i, lv := range levels {
		if i > 0 {
			fmt.Fprintln(w)
		}
		marker := ""
		if lv.isInvocation {
			marker = " (invocation)"
		}
		fmt.Fprintf(w, "# === [%s] %s%s ===\n", lv.inv.FsHash, lv.afPath, marker)
		if _, err := w.Write(renderState(lv.state)); err != nil {
			return err
		}
		// Workspace files belong to the level you are actually in: they are
		// written into the invocation's own directory and never travel to a
		// federation parent. They also never reach alpha, so they are resolved
		// here rather than read back out of StateInfo.
		if !lv.isInvocation {
			continue
		}
		spec, err := alphasfile.RenderWorkspace(lv.afPath, lv.inv)
		if err != nil {
			return err
		}
		if _, err := w.Write(renderWorkspaceBlock(spec)); err != nil {
			return err
		}
	}
	return nil
}

// renderWorkspaceBlock writes the resolved top-level workspace block: the
// branch template as written, and each file with its content already
// substituted — what `zordon workspace apply` would put on disk.
func renderWorkspaceBlock(spec *alphasfile.WorkspaceSpec) []byte {
	f := hclwrite.NewFile()
	// Nothing declared ⇒ nothing to render. Emitting the default template into
	// every plan would be noise in projects that never asked for a workspace
	// block at all.
	if spec == nil || (len(spec.Files) == 0 && spec.BranchTemplate() == alphasfile.DefaultBranchTemplate) {
		return f.Bytes()
	}
	body := f.Body()
	wb := body.AppendNewBlock("workspace", nil).Body()
	wb.SetAttributeValue("branch", cty.StringVal(spec.BranchTemplate()))
	for _, wf := range spec.Files {
		fb := wb.AppendNewBlock("file", []string{wf.Name}).Body()
		fb.SetAttributeValue("path", cty.StringVal(wf.Path))
		ob := fb.AppendNewBlock(string(wf.Op), nil).Body()
		switch wf.Op {
		case alphasfile.OpCreate:
			ob.SetAttributeValue("body", cty.StringVal(wf.Body))
		case alphasfile.OpRegion:
			ob.SetAttributeValue("body", cty.StringVal(wf.Body))
			if wf.Comment != "" {
				ob.SetAttributeValue("comment", cty.StringVal(wf.Comment))
			}
		case alphasfile.OpMerge:
			ob.SetAttributeValue("format", cty.StringVal(string(wf.Format)))
			// data is rendered as the encoded document rather than as an HCL
			// object: it is what actually gets merged, and it round-trips
			// through a golden diff unambiguously.
			encoded, err := zdoc.Encode(wf.Data, wf.Format)
			if err != nil {
				encoded = fmt.Appendf(nil, "<unencodable: %v>", err)
			}
			ob.SetAttributeValue("data", cty.StringVal(string(encoded)))
		}
	}
	return f.Bytes()
}

// renderState writes a resolved StateInfo back as HCL bytes, every
// interpolation already substituted. Output is for human inspection
// and golden-file diffs, not round-tripping: block ordering follows
// the resolved struct layout rather than the original source.
func renderState(st *protocol.StateInfo) []byte {
	f := hclwrite.NewFile()
	body := f.Body()
	if st == nil {
		return f.Bytes()
	}

	if len(st.Dotenv) > 0 {
		body.SetAttributeValue("dotenv", stringListVal(st.Dotenv))
	}
	if len(st.Env) > 0 {
		body.SetAttributeValue("env", mapStringStringVal(st.Env))
	}
	if len(st.SysEnv) > 0 {
		body.SetAttributeValue("sysenv", stringListVal(st.SysEnv))
	}
	if len(st.Toolchain) > 0 {
		renderToolchainBlock(body, st.Toolchain)
	}
	for _, s := range st.Services {
		renderService(body, s)
	}
	return f.Bytes()
}

func renderToolchainBlock(body *hclwrite.Body, tc map[string]*alphasfile.ToolchainConfig) {
	tb := body.AppendNewBlock("toolchain", nil).Body()
	for _, lang := range []string{alphasfile.ToolchainGo, alphasfile.ToolchainRust, alphasfile.ToolchainRuby, alphasfile.ToolchainNode, alphasfile.ToolchainJava} {
		cfg, ok := tc[lang]
		if !ok || cfg == nil {
			continue
		}
		lb := tb.AppendNewBlock(lang, nil).Body()
		lb.SetAttributeValue("version", cty.StringVal(cfg.Version))
		if len(cfg.Tools) > 0 {
			lb.SetAttributeValue("tools", mapStringStringVal(cfg.Tools))
		}
		if len(cfg.Env) > 0 {
			lb.SetAttributeValue("env", mapStringStringVal(cfg.Env))
		}
	}
}

func renderService(body *hclwrite.Body, s *alphasfile.Service) {
	if s == nil || s.Runtime == nil {
		return
	}
	rt := s.Runtime
	sb := body.AppendNewBlock("service", []string{s.Toolchain, rt.Name}).Body()

	if s.Package != nil {
		renderSource(sb, s.Package)
	}
	if s.Pkg != nil {
		sb.SetAttributeValue("package", mapStringStringVal(map[string]string{
			"name":    s.Pkg.Name,
			"version": s.Pkg.Version,
		}))
	}

	if rt.Color != "" {
		sb.SetAttributeValue("color", cty.StringVal(rt.Color))
	}
	if len(rt.Vars) > 0 {
		sb.SetAttributeValue("vars", mapAnyVal(rt.Vars))
	}
	hasOpts := rt.Options != nil && (rt.Options.Prefix != nil || rt.Options.Separator != nil)
	if len(rt.Arguments) > 0 || hasOpts {
		ab := sb.AppendNewBlock("arguments", nil).Body()
		if len(rt.Arguments) > 0 {
			ab.SetAttributeValue("values", groupsToCty(rt.Arguments))
		}
		if hasOpts {
			ob := ab.AppendNewBlock("options", nil).Body()
			if rt.Options.Prefix != nil {
				ob.SetAttributeValue("prefix", cty.StringVal(*rt.Options.Prefix))
			}
			if rt.Options.Separator != nil {
				ob.SetAttributeValue("separator", cty.StringVal(*rt.Options.Separator))
			}
		}
	}
	if len(rt.Env) > 0 {
		sb.SetAttributeValue("env", mapStringStringVal(rt.Env))
	}
	if len(rt.Dotenv) > 0 {
		sb.SetAttributeValue("dotenv", stringListVal(rt.Dotenv))
	}
	if rt.Print != "" {
		sb.SetAttributeValue("print", cty.StringVal(rt.Print))
	}

	if rt.Log != nil && (rt.Log.Format != "" || rt.Log.Filter != "" || rt.Log.TTY != nil) {
		lb := sb.AppendNewBlock("log", nil).Body()
		if rt.Log.Format != "" {
			lb.SetAttributeValue("format", cty.StringVal(rt.Log.Format))
		}
		if rt.Log.Filter != "" {
			lb.SetAttributeValue("filter", cty.StringVal(rt.Log.Filter))
		}
		if rt.Log.TTY != nil {
			lb.SetAttributeValue("tty", cty.BoolVal(*rt.Log.TTY))
		}
	}

	if (s.Package != nil && len(s.Package.BuildCmd) > 0) || len(rt.BuildEnv) > 0 {
		bb := sb.AppendNewBlock("build", nil).Body()
		if s.Package != nil && len(s.Package.BuildCmd) > 0 {
			bb.SetAttributeValue("cmd", stringListVal(s.Package.BuildCmd))
		}
		if len(rt.BuildEnv) > 0 {
			bb.SetAttributeValue("env", mapStringStringVal(rt.BuildEnv))
		}
	}

	if len(rt.Command) > 0 || len(rt.RunEnv) > 0 || len(rt.After) > 0 || len(rt.Provision) > 0 {
		rb := sb.AppendNewBlock("runtime", nil).Body()
		if len(rt.Command) > 0 {
			rb.SetAttributeValue("cmd", stringListVal(rt.Command))
		}
		if len(rt.RunEnv) > 0 {
			rb.SetAttributeValue("env", mapStringStringVal(rt.RunEnv))
		}
		if len(rt.After) > 0 {
			rb.SetAttributeValue("after", stringListVal(rt.After))
		}
		for _, p := range rt.Provision {
			renderProvision(rb, p)
		}
	}

	if len(rt.AgentEnv) > 0 {
		ab := sb.AppendNewBlock("agent", nil).Body()
		ab.SetAttributeValue("env", mapStringStringVal(rt.AgentEnv))
	}

	if s.Debugger != nil && (s.Debugger.Enabled || s.Debugger.Port != 0) {
		db := sb.AppendNewBlock("debugger", nil).Body()
		db.SetAttributeValue("enabled", cty.BoolVal(s.Debugger.Enabled))
		if s.Debugger.Port != 0 {
			db.SetAttributeValue("port", cty.NumberIntVal(int64(s.Debugger.Port)))
		}
		if s.Debugger.WaitForClient {
			db.SetAttributeValue("wait_for_client", cty.BoolVal(true))
		}
		if s.Debugger.Log {
			db.SetAttributeValue("log", cty.BoolVal(true))
		}
		if s.Debugger.MCP {
			db.SetAttributeValue("mcp", cty.BoolVal(true))
		}
		if s.Debugger.WrapRuntime {
			db.SetAttributeValue("wrap_runtime", cty.BoolVal(true))
		}
	}

	if s.Package != nil && s.Package.Workspace != nil && len(s.Package.Workspace.Sparse) > 0 {
		wb := sb.AppendNewBlock("workspace", nil).Body()
		wb.SetAttributeValue("sparse", stringListVal(s.Package.Workspace.Sparse))
	}

	for _, st := range rt.Sudo {
		sub := sb.AppendNewBlock("sudo", []string{st.Name}).Body()
		if st.Check != "" {
			sub.SetAttributeValue("check", cty.StringVal(st.Check))
		}
		sub.SetAttributeValue("apply", cty.StringVal(st.Apply))
		if st.Verify != "" {
			sub.SetAttributeValue("verify", cty.StringVal(st.Verify))
		}
	}

	for _, fl := range rt.Files {
		fb := sb.AppendNewBlock("file", []string{fl.Name}).Body()
		fb.SetAttributeValue("path", cty.StringVal(fl.Path))
		fb.SetAttributeValue("body", cty.StringVal(fl.Body))
	}

	if rt.Readiness != nil {
		renderReadiness(sb, rt.Readiness)
	}
}

func renderSource(sb *hclwrite.Body, pkg *alphasfile.Package) {
	switch {
	case pkg.Install != "" && pkg.Toolchain == alphasfile.ToolchainRust:
		cb := sb.AppendNewBlock("crate", nil).Body()
		cb.SetAttributeValue("name", cty.StringVal(pkg.Install))
		if pkg.Version != "" {
			cb.SetAttributeValue("version", cty.StringVal(pkg.Version))
		}
		if pkg.Index != "" {
			cb.SetAttributeValue("index", cty.StringVal(pkg.Index))
		}
		if pkg.Registry != "" {
			cb.SetAttributeValue("registry", cty.StringVal(pkg.Registry))
		}
	case pkg.Install != "":
		// go use-only install: `package = "<pkg>@<ver>"` at service level.
		sb.SetAttributeValue("package", cty.StringVal(pkg.Install))
	case pkg.Git != "":
		gb := sb.AppendNewBlock("git", nil).Body()
		gb.SetAttributeValue("url", cty.StringVal(pkg.Git))
		if pkg.Branch != "" {
			gb.SetAttributeValue("branch", cty.StringVal(pkg.Branch))
		}
		if pkg.Tag != "" {
			gb.SetAttributeValue("tag", cty.StringVal(pkg.Tag))
		}
		if pkg.Rev != "" {
			gb.SetAttributeValue("rev", cty.StringVal(pkg.Rev))
		}
		if pkg.Exe != "" || pkg.Src != "" {
			srb := sb.AppendNewBlock("src", nil).Body()
			if pkg.Src != "" {
				srb.SetAttributeValue("path", cty.StringVal(pkg.Src))
			}
			if pkg.Exe != "" {
				srb.SetAttributeValue("exe", cty.StringVal(pkg.Exe))
			}
		}
	case pkg.Src != "":
		srb := sb.AppendNewBlock("src", nil).Body()
		srb.SetAttributeValue("path", cty.StringVal(pkg.Src))
		if pkg.Exe != "" {
			srb.SetAttributeValue("exe", cty.StringVal(pkg.Exe))
		}
	}
	if pkg.Bin != "" {
		sb.SetAttributeValue("bin", cty.StringVal(pkg.Bin))
	}
	if len(pkg.Features) > 0 {
		sb.SetAttributeValue("features", stringListVal(pkg.Features))
	}
}

func renderProvision(rb *hclwrite.Body, p *alphasfile.ProvisionStep) {
	sub := rb.AppendNewBlock("provision", []string{p.Name}).Body()
	if p.Check != "" {
		sub.SetAttributeValue("check", cty.StringVal(p.Check))
	}
	if p.Cmd != "" {
		sub.SetAttributeValue("cmd", cty.StringVal(p.Cmd))
	}
	if p.CmdRef != "" {
		sub.SetAttributeValue("cmd_ref", cty.StringVal(p.CmdRef))
	}
	if p.Verify != "" {
		sub.SetAttributeValue("verify", cty.StringVal(p.Verify))
	}
	if len(p.Env) > 0 {
		sub.SetAttributeValue("env", mapStringStringVal(p.Env))
	}
	switch {
	case p.Latent:
		// Source HCL spelled this `after = never` (identifier). hclwrite
		// can't emit a bare identifier via SetAttributeValue, so render
		// the resolved fact as a quoted sentinel — unambiguous to humans.
		sub.SetAttributeValue("after", cty.StringVal("never"))
	case len(p.After) > 0:
		sub.SetAttributeValue("after", stringListVal(p.After))
	}
	if p.Detached {
		sub.SetAttributeValue("detached", cty.BoolVal(true))
	}
}

func renderReadiness(sb *hclwrite.Body, p *probe.Probe) {
	rb := sb.AppendNewBlock("readiness", nil).Body()
	if p.HTTP != nil {
		hb := rb.AppendNewBlock("http", nil).Body()
		if p.HTTP.Path != "" {
			hb.SetAttributeValue("path", cty.StringVal(p.HTTP.Path))
		}
		hb.SetAttributeValue("port", cty.NumberIntVal(int64(p.HTTP.Port)))
		if p.HTTP.Host != "" {
			hb.SetAttributeValue("host", cty.StringVal(p.HTTP.Host))
		}
		if p.HTTP.Scheme != "" {
			hb.SetAttributeValue("scheme", cty.StringVal(p.HTTP.Scheme))
		}
	}
	if p.Exec != nil {
		eb := rb.AppendNewBlock("exec", nil).Body()
		eb.SetAttributeValue("command", stringListVal(p.Exec.Command))
		if len(p.Exec.Env) > 0 {
			eb.SetAttributeValue("env", mapStringStringVal(p.Exec.Env))
		}
	}
	if p.TCP != nil {
		tb := rb.AppendNewBlock("tcp", nil).Body()
		tb.SetAttributeValue("port", cty.NumberIntVal(int64(p.TCP.Port)))
		if p.TCP.Host != "" {
			tb.SetAttributeValue("host", cty.StringVal(p.TCP.Host))
		}
	}
	if p.InitialDelay != 0 {
		rb.SetAttributeValue("initial_delay", cty.StringVal(p.InitialDelay.String()))
	}
	if p.Period != 0 {
		rb.SetAttributeValue("period", cty.StringVal(p.Period.String()))
	}
	if p.Timeout != 0 {
		rb.SetAttributeValue("timeout", cty.StringVal(p.Timeout.String()))
	}
	if p.FailureThreshold != 0 {
		rb.SetAttributeValue("failure_threshold", cty.NumberIntVal(int64(p.FailureThreshold)))
	}
	if p.SuccessThreshold != 0 {
		rb.SetAttributeValue("success_threshold", cty.NumberIntVal(int64(p.SuccessThreshold)))
	}
}

// --- value coercion helpers ----------------------------------------------

func stringListVal(ss []string) cty.Value {
	if len(ss) == 0 {
		return cty.ListValEmpty(cty.String)
	}
	vs := make([]cty.Value, len(ss))
	for i, s := range ss {
		vs[i] = cty.StringVal(s)
	}
	return cty.ListVal(vs)
}

func mapStringStringVal(m map[string]string) cty.Value {
	if len(m) == 0 {
		return cty.EmptyObjectVal
	}
	out := make(map[string]cty.Value, len(m))
	for k, v := range m {
		out[k] = cty.StringVal(v)
	}
	return cty.ObjectVal(out)
}

func mapAnyVal(m map[string]any) cty.Value {
	if len(m) == 0 {
		return cty.EmptyObjectVal
	}
	out := make(map[string]cty.Value, len(m))
	for k, v := range m {
		out[k] = anyToCty(v)
	}
	return cty.ObjectVal(out)
}

// groupsToCty renders resolved argument groups (name → flag map) as a
// cty object-of-objects for the plan's `arguments { values = {…} }` block.
func groupsToCty(groups map[string]map[string]any) cty.Value {
	if len(groups) == 0 {
		return cty.EmptyObjectVal
	}
	out := make(map[string]cty.Value, len(groups))
	for name, flags := range groups {
		out[name] = mapAnyVal(flags)
	}
	return cty.ObjectVal(out)
}

// anyToCty turns a JSON-decoded Go scalar into a cty.Value. Used to
// project resolved RuntimeConfig fields (vars / arguments) — whose
// concrete type was lost through their `map[string]any` shape — back
// into the typed value space hclwrite needs.
func anyToCty(v any) cty.Value {
	switch x := v.(type) {
	case nil:
		return cty.NullVal(cty.String)
	case string:
		return cty.StringVal(x)
	case bool:
		return cty.BoolVal(x)
	case int:
		return cty.NumberIntVal(int64(x))
	case int64:
		return cty.NumberIntVal(x)
	case float64:
		if x == float64(int64(x)) {
			return cty.NumberIntVal(int64(x))
		}
		return cty.NumberFloatVal(x)
	case []any:
		if len(x) == 0 {
			return cty.EmptyTupleVal
		}
		vs := make([]cty.Value, len(x))
		for i, e := range x {
			vs[i] = anyToCty(e)
		}
		return cty.TupleVal(vs)
	case map[string]any:
		if len(x) == 0 {
			return cty.EmptyObjectVal
		}
		out := make(map[string]cty.Value, len(x))
		for k, e := range x {
			out[k] = anyToCty(e)
		}
		return cty.ObjectVal(out)
	}
	return cty.StringVal(fmt.Sprintf("%v", v))
}

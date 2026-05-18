package alphasfile

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/lifecycle"
)

// buildBarrierAttrs returns one cty.StringVal per state, each holding
// the canonical "<entityID>@<state>" barrier ref. Used by evalService
// to give cty objects a referenceable status surface — HCL traversals
// like `service.go.api.ready` ultimately resolve to the matching ref
// string, which alpha then maps back to a real *barrier.Barrier at
// bringup time.
func buildBarrierAttrs(entityID string, states []lifecycle.State) map[string]cty.Value {
	attrs := make(map[string]cty.Value, len(states))
	for _, s := range states {
		attrs[string(s)] = cty.StringVal(entityID + "@" + string(s))
	}
	return attrs
}

// resolver walks the service DAG and produces the wire-stable *Alphasfile.
//
// Within each service, evaluation goes through a fixed sequence so that
// `self` can grow incrementally:
//
//	1. self_base   = { name, toolchain, dir }
//	2. eval Vars   → self.vars  = {...}
//	3. eval Files  → self.file.<name> = { path, body }
//	4. eval Args   → self.arguments = {...}
//	5. eval Probe  → readiness port
//
// Files come before arguments because the common pattern is to generate a
// config file and pass its path as a flag. Going the other way (file body
// referencing argument values) is uncommon and can be expressed via vars.
type resolver struct {
	path string
	root *rootBlock
	inv  *invocation.Invocation

	// Already-evaluated services, keyed by toolchain then name. Re-projected
	// into the EvalContext under "service" before each new eval step. May be
	// pre-seeded with parent services for federation (flat namespace).
	serviceByTC map[string]map[string]cty.Value

	// names already taken (parent or local); collision is an error.
	taken map[string]string // "tc/name" → origin ("parent" | "local")

	// checkout dir of the service currently being evaluated (fs::src()).
	curCheckout string

	resolvedServices []*Service
}

func resolve(name string, root *rootBlock, inv *invocation.Invocation, seed map[string]map[string]cty.Value) (*Alphasfile, error) {
	if seed == nil {
		seed = map[string]map[string]cty.Value{}
	}
	r := &resolver{
		path:        name,
		root:        root,
		inv:         inv,
		serviceByTC: seed,
		taken:       map[string]string{},
	}
	// ... rest of resolve function


	parentKnown := map[string]struct{}{}
	for tc, byName := range seed {
		for name := range byName {
			r.taken[tc+"/"+name] = "parent"
			parentKnown[serviceID(tc, name)] = struct{}{}
		}
	}
	g, err := newGraph(root.Services, parentKnown)
	if err != nil {
		return nil, err
	}
	order, err := g.topoSort()
	if err != nil {
		return nil, err
	}
	for _, n := range order {
		if err := r.evalService(n.service); err != nil {
			return nil, fmt.Errorf("%s: %w", n.id, err)
		}
	}
	// File-level dotenv: top-level, no `self`; may use fs::/cfg:: funcs.
	gdot, err := r.evalStr(root.Dotenv, nil, "dotenv")
	if err != nil {
		return nil, err
	}
	return &Alphasfile{Dotenv: gdot, Services: r.resolvedServices}, nil
}

func (r *resolver) evalService(sb *serviceBlock) error {
	// A local service with the same name as a federation-parent service
	// COMPLETELY overrides it: the child definition replaces the parent's
	// in this level's namespace (and runs in this level's alpha). Two
	// local blocks with the same name in one file is still a real mistake.
	if origin := r.taken[sb.Toolchain+"/"+sb.Name]; origin == "local" {
		return fmt.Errorf("duplicate service %s.%s in this Alphasfile", sb.Toolchain, sb.Name)
	}
	r.taken[sb.Toolchain+"/"+sb.Name] = "local"

	// dir = where this service's code lives for this invocation.
	//
	//   src-only in the "main" worktree → the src dir itself, used IN
	//     PLACE (your live working tree; zordon never copies or git-
	//     worktrees it, so uncommitted edits just work — the edit→start
	//     loop). "main" is only a mental label; in practice it means
	//     "use src directly".
	//   anything else (named worktree, or a git primary) → a per-
	//     invocation checkout alpha materializes via git worktree add.
	//
	// Pure: no filesystem touch here.
	dir := ""
	switch {
	case sb.Src != "" && sb.Git == "" && r.inv.Worktree == invocation.MainWorktree:
		dir = r.resolveDir(sb.Src)
	case sb.Git != "" || sb.Src != "":
		dir = r.inv.CheckoutPath(sb.Name)
	}
	r.curCheckout = dir // what fs::src() returns while this service evals

	// self_base — known from labels + primary, before any eval. Includes
	// the static barrier-status attrs (scheduled/running/ready/stopped/
	// done) and the runtime.provision.<name>.<status> sub-objects so HCL
	// traversals like `self.ready` or `self.runtime.provision.x.success`
	// resolve to canonical "<entityID>@<state>" ref strings during eval.
	serviceID := "service." + sb.Toolchain + "." + sb.Name
	self := map[string]cty.Value{
		"name":      cty.StringVal(sb.Name),
		"toolchain": cty.StringVal(sb.Toolchain),
		"dir":       cty.StringVal(dir),
	}
	for k, v := range buildBarrierAttrs(serviceID, ServiceBarrierStates) {
		self[k] = v
	}
	if sb.Runtime != nil && len(sb.Runtime.Provision) > 0 {
		provObjs := make(map[string]cty.Value, len(sb.Runtime.Provision))
		for _, pb := range sb.Runtime.Provision {
			provID := serviceID + ".runtime.provision." + pb.Name
			provObjs[pb.Name] = cty.ObjectVal(buildBarrierAttrs(provID, ProvisionBarrierStates))
		}
		self["runtime"] = cty.ObjectVal(map[string]cty.Value{
			"provision": cty.ObjectVal(provObjs),
		})
	}

	// Producers (vars, each file, arguments) are evaluated in dependency
	// order, not a fixed sequence: edges come from self.<x> traversals in
	// each expression, so `arguments` referencing self.file.cfg.path and a
	// file body referencing self.vars.port both just work — whichever way
	// you write it. A genuine mutual reference is a clear cycle error.
	var (
		varsVal  map[string]any
		args     map[string]any
		envMap   map[string]any
		files    []*File
		fileVals = map[string]cty.Value{}
	)
	type producer struct {
		id    string
		exprs []hcl.Expression
		run   func() error
	}
	prods := []*producer{
		{id: "vars", exprs: []hcl.Expression{sb.Vars}, run: func() error {
			v, err := r.evalMap(sb.Vars, self, "vars")
			if err != nil {
				return err
			}
			varsVal = v
			self["vars"] = mapValToCty(v)
			return nil
		}},
	}
	for _, fb := range sb.Files {
		fb := fb
		prods = append(prods, &producer{
			id:    "file." + fb.Name,
			exprs: []hcl.Expression{fb.Path, fb.Body},
			run: func() error {
				path, body, err := evalFileExpr(fb, r.ctxWith(self))
				if err != nil {
					return err
				}
				files = append(files, &File{Name: fb.Name, Path: path, Body: body})
				fileVals[fb.Name] = cty.ObjectVal(map[string]cty.Value{
					"name": cty.StringVal(fb.Name),
					"path": cty.StringVal(path),
					"body": cty.StringVal(body),
				})
				self["file"] = cty.ObjectVal(fileVals)
				return nil
			},
		})
	}
	prods = append(prods, &producer{id: "arguments", exprs: []hcl.Expression{sb.Arguments}, run: func() error {
		a, err := r.evalMap(sb.Arguments, self, "arguments")
		if err != nil {
			return err
		}
		args = a
		self["arguments"] = mapValToCty(a)
		return nil
	}})
	prods = append(prods, &producer{id: "env", exprs: []hcl.Expression{sb.Env}, run: func() error {
		e, err := r.evalMap(sb.Env, self, "env")
		if err != nil {
			return err
		}
		envMap = e
		self["env"] = mapValToCty(e)
		return nil
	}})

	byID := make(map[string]*producer, len(prods))
	declOrder := make([]string, 0, len(prods))
	for _, p := range prods {
		byID[p.id] = p
		declOrder = append(declOrder, p.id)
	}
	deps := map[string]map[string]struct{}{}
	for _, p := range prods {
		deps[p.id] = map[string]struct{}{}
		for _, e := range p.exprs {
			if e == nil {
				continue
			}
			for _, t := range e.Variables() {
				if dep, ok := selfProducerID(t); ok && dep != p.id {
					if _, real := byID[dep]; real {
						deps[p.id][dep] = struct{}{}
					}
				}
			}
		}
	}
	order, err := topoProducers(declOrder, deps)
	if err != nil {
		return fmt.Errorf("service %q: %w", sb.Name, err)
	}
	for _, id := range order {
		if err := byID[id].run(); err != nil {
			return err
		}
	}
	if _, ok := self["file"]; !ok {
		self["file"] = cty.EmptyObjectVal
	}

	// Sinks: consume the fully-populated self; order among them is
	// irrelevant since nothing reads them back.
	var command []string
	if sb.Runtime != nil && sb.Runtime.Cmd != nil {
		command, err = r.evalStrList(sb.Runtime.Cmd, self, "runtime.cmd")
		if err != nil {
			return err
		}
	}
	var buildCmd []string
	if sb.Build != nil && sb.Build.Cmd != nil {
		buildCmd, err = r.evalStrList(sb.Build.Cmd, self, "build.cmd")
		if err != nil {
			return err
		}
	}
	phaseEnv := func(expr hcl.Expression, label string) (map[string]string, error) {
		if expr == nil {
			return nil, nil
		}
		m, e := r.evalMap(expr, self, label)
		if e != nil {
			return nil, e
		}
		return toStringMap(m), nil
	}
	var buildEnv, runEnv, agentEnv map[string]string
	if sb.Build != nil {
		if buildEnv, err = phaseEnv(sb.Build.Env, "build.env"); err != nil {
			return err
		}
	}
	if sb.Runtime != nil {
		if runEnv, err = phaseEnv(sb.Runtime.Env, "runtime.env"); err != nil {
			return err
		}
	}
	if sb.Agent != nil {
		if agentEnv, err = phaseEnv(sb.Agent.Env, "agent.env"); err != nil {
			return err
		}
	}
	dotenv, err := r.evalStr(sb.Dotenv, self, "dotenv")
	if err != nil {
		return err
	}
	printLine, err := r.evalStr(sb.Print, self, "print")
	if err != nil {
		return err
	}
	// sudo hooks (may reference everything resolved so far).
	var sudo []*SudoStep
	for _, sblk := range sb.Sudo {
		check, err := r.evalStr(sblk.Check, self, "sudo."+sblk.Name+".check")
		if err != nil {
			return err
		}
		apply, err := r.evalStr(sblk.Apply, self, "sudo."+sblk.Name+".apply")
		if err != nil {
			return err
		}
		verify, err := r.evalStr(sblk.Verify, self, "sudo."+sblk.Name+".verify")
		if err != nil {
			return err
		}
		sudo = append(sudo, &SudoStep{Name: sblk.Name, Check: check, Apply: apply, Verify: verify})
	}

	// provision hooks: same shape as sudo plus env + after + detached.
	// after is a list of barrier refs (canonical "<entityID>@<state>"
	// strings); alpha parses them at bringup and selects on the matching
	// barriers. References can be local (self.runtime.provision.X.<s>)
	// or cross-service (service.<tc>.<svc>.<s>).
	var provisions []*ProvisionStep
	if sb.Runtime != nil {
		for _, pb := range sb.Runtime.Provision {
			label := "provision." + pb.Name
			check, err := r.evalStr(pb.Check, self, label+".check")
			if err != nil {
				return err
			}
			cmd, err := r.evalStr(pb.Cmd, self, label+".cmd")
			if err != nil {
				return err
			}
			verify, err := r.evalStr(pb.Verify, self, label+".verify")
			if err != nil {
				return err
			}
			var penv map[string]string
			if pb.Env != nil {
				m, err := r.evalMap(pb.Env, self, label+".env")
				if err != nil {
					return err
				}
				penv = toStringMap(m)
			}
			afterList, err := r.evalStrList(pb.After, self, label+".after")
			if err != nil {
				return err
			}
			provisions = append(provisions, &ProvisionStep{
				Name:     pb.Name,
				Check:    check,
				Cmd:      cmd,
				Verify:   verify,
				Env:      penv,
				After:    afterList,
				Detached: pb.Detached,
			})
		}
	}

	// Stage 7: readiness port (may reference everything in self).
	var probePort int
	if sb.Readiness != nil {
		ctx := r.ctxWith(self)
		var (
			expr  hcl.Expression
			field string
		)
		switch {
		case sb.Readiness.HTTP != nil:
			expr, field = sb.Readiness.HTTP.Port, "readiness.http.port"
		}
		if expr != nil {
			probePort, err = evalIntExpr(expr, ctx, field)
			if err != nil {
				return err
			}
		}
	}

	// Build the wire-stable Service and runtime config.
	rt := &RuntimeConfig{
		Name:           sb.Name,
		Color:          sb.Color,
		DoubleDash:     sb.DoubleDash,
		SpaceSeparated: sb.SpaceSeparated,
		Vars:           varsVal,
		Arguments:      args,
		Env:            toStringMap(envMap),
		BuildEnv:       buildEnv,
		RunEnv:         runEnv,
		AgentEnv:       agentEnv,
		Dotenv:         dotenv,
		Command:        command,
		Sudo:           sudo,
		Provision:      provisions,
		Files:          files,
		Dir:            dir,
		BinDir:         r.inv.BinDir(),
		Print:          printLine,
	}
	// Resolve per-toolchain defaults so the wire-stable Service is fully
	// populated — alpha can read rt.Log.TTY without knowing what
	// "ruby" means.
	rt.Log = &LogConfig{}
	if sb.Log != nil {
		rt.Log.Format = sb.Log.Format
		rt.Log.Filter = sb.Log.Filter
		rt.Log.TTY = sb.Log.TTY
	}
	if rt.Log.TTY == nil {
		def := toolchainDefaultsFor[sb.Toolchain].TTY
		rt.Log.TTY = &def
	}
	if sb.Readiness != nil {
		p, err := compileProbe(sb.Readiness, probePort)
		if err != nil {
			return fmt.Errorf("readiness: %w", err)
		}
		rt.Readiness = p
	}

	switch sb.Toolchain {
	case ToolchainGo, ToolchainRust, ToolchainRuby:
	default:
		return fmt.Errorf("unknown toolchain %q (want go|rust|ruby)", sb.Toolchain)
	}
	// Use-only: the install coordinate is read from the field that
	// matches the toolchain (`package` for go, `cargo` for rust).
	// Cross-toolchain fields are a mistake.
	var install, instVersion string
	var features []string
	switch sb.Toolchain {
	case ToolchainGo:
		if sb.Cargo != "" || len(sb.Features) > 0 {
			return fmt.Errorf("service %q is go but declares rust use-only fields (cargo/features)", sb.Name)
		}
		install = strings.TrimSpace(sb.Package)
	case ToolchainRust:
		if sb.Package != "" {
			return fmt.Errorf("service %q is rust but declares go use-only field (package)", sb.Name)
		}
		install = strings.TrimSpace(sb.Cargo)
		features = sb.Features
		instVersion = strings.TrimSpace(sb.Version)
	default:
		if sb.Package != "" || sb.Cargo != "" {
			return fmt.Errorf("service %q: %s has no use-only mode", sb.Name, sb.Toolchain)
		}
	}

	// Modes: use-only (install) XOR worktree (src, optionally seeded by git).
	src := sb.Src
	switch {
	case install != "":
		if sb.Git != "" || src != "" {
			return fmt.Errorf("service %q declares use-only (%s) together with git/src; pick one", sb.Name, sb.Toolchain)
		}
	case src == "" && sb.Git == "":
		field := "package"
		if sb.Toolchain == ToolchainRust {
			field = "cargo"
		}
		return fmt.Errorf("service %q has no source: declare src, git, or %s (use-only)", sb.Name, field)
	}

	var worktree *Worktree
	if sb.Worktree != nil && len(sb.Worktree.Sparse) > 0 {
		// Sparse cone paths are relative to the primary repo root (what
		// `git sparse-checkout set` expects) — pass verbatim, not joined
		// to an absolute Alphasfile path.
		worktree = &Worktree{Sparse: cleanSparse(sb.Worktree.Sparse)}
	}

	svc := &Service{
		Toolchain: sb.Toolchain,
		Runtime:   rt,
		Package: &Package{
			Toolchain: sb.Toolchain,
			Git:       sb.Git,
			Src:       r.resolveDir(src),
			Branch:    sb.Branch,
			Tag:       sb.Tag,
			Rev:       sb.Rev,
			Install:   install,
			Version:   instVersion,
			Features:  features,
			Exe:       sb.Exe,
			Bin:       sb.Bin,
			BuildCmd:  buildCmd,
			Cmd:       strings.Join(command, " "),
			Worktree:  worktree,
			// In-place: src-only in the "main" worktree → alpha builds/runs
			// from src as-is (no git worktree add, no HEAD reset).
			InPlace: sb.Src != "" && sb.Git == "" && r.inv.Worktree == invocation.MainWorktree,
		},
	}
	r.resolvedServices = append(r.resolvedServices, svc)

	// Expose the fully-evaluated service to downstream blocks.
	if r.serviceByTC[sb.Toolchain] == nil {
		r.serviceByTC[sb.Toolchain] = map[string]cty.Value{}
	}
	r.serviceByTC[sb.Toolchain][sb.Name] = cty.ObjectVal(self)
	return nil
}

// evalMap evaluates an HCL expression that should yield a map/object and
// returns its members as a Go map (for embedding in RuntimeConfig) plus
// nil-safety. Used for both `vars` and `arguments`.
func (r *resolver) evalMap(expr hcl.Expression, self map[string]cty.Value, field string) (map[string]any, error) {
	if expr == nil {
		return nil, nil
	}
	ctx := r.ctxWith(self)
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: %s", field, diags.Error())
	}
	if val.IsNull() {
		return nil, nil
	}
	t := val.Type()
	if !t.IsObjectType() && !t.IsMapType() {
		return nil, fmt.Errorf("%s must be a map/object, got %s", field, t.FriendlyName())
	}
	out := map[string]any{}
	for k, v := range val.AsValueMap() {
		out[k] = ctyToAny(v)
	}
	return out, nil
}

// selfProducerID maps a `self.<x>...` traversal to the intra-service
// producer node it depends on: self.vars → "vars", self.arguments →
// "arguments", self.file.<n> → "file.<n>". self.{name,toolchain,dir} and
// non-self roots are not producers.
func selfProducerID(t hcl.Traversal) (string, bool) {
	if len(t) < 2 {
		return "", false
	}
	root, ok := t[0].(hcl.TraverseRoot)
	if !ok || root.Name != "self" {
		return "", false
	}
	a1, ok := t[1].(hcl.TraverseAttr)
	if !ok {
		return "", false
	}
	switch a1.Name {
	case "vars":
		return "vars", true
	case "arguments":
		return "arguments", true
	case "file":
		if len(t) >= 3 {
			if a2, ok := t[2].(hcl.TraverseAttr); ok {
				return "file." + a2.Name, true
			}
		}
		return "file", true // whole self.file ⇒ depends on all files
	}
	return "", false
}

// topoProducers orders producer ids so every node comes after its deps.
// declOrder (the source declaration order) breaks ties → stable and
// deterministic. A cycle is a clear error naming the entangled nodes.
func topoProducers(declOrder []string, deps map[string]map[string]struct{}) ([]string, error) {
	indeg := make(map[string]int, len(declOrder))
	for _, id := range declOrder {
		indeg[id] = len(deps[id])
	}
	done := map[string]bool{}
	out := make([]string, 0, len(declOrder))
	for len(out) < len(declOrder) {
		progressed := false
		for _, id := range declOrder { // declaration order = stable tiebreak
			if done[id] || indeg[id] != 0 {
				continue
			}
			out = append(out, id)
			done[id] = true
			progressed = true
			for _, other := range declOrder {
				if _, ok := deps[other][id]; ok {
					indeg[other]--
				}
			}
		}
		if !progressed {
			var stuck []string
			for _, id := range declOrder {
				if !done[id] {
					stuck = append(stuck, id)
				}
			}
			return nil, fmt.Errorf("cyclic references among %s — these depend on each other",
				strings.Join(stuck, ", "))
		}
	}
	return out, nil
}

func evalFileExpr(fb *fileBlock, ctx *hcl.EvalContext) (string, string, error) {
	pathVal, diags := fb.Path.Value(ctx)
	if diags.HasErrors() {
		return "", "", fmt.Errorf("file.%s.path: %s", fb.Name, diags.Error())
	}
	if pathVal.Type() != cty.String {
		return "", "", fmt.Errorf("file.%s.path must be a string, got %s", fb.Name, pathVal.Type().FriendlyName())
	}
	bodyVal, diags := fb.Body.Value(ctx)
	if diags.HasErrors() {
		return "", "", fmt.Errorf("file.%s.body: %s", fb.Name, diags.Error())
	}
	if bodyVal.Type() != cty.String {
		return "", "", fmt.Errorf("file.%s.body must be a string, got %s", fb.Name, bodyVal.Type().FriendlyName())
	}
	return pathVal.AsString(), bodyVal.AsString(), nil
}

func evalIntExpr(expr hcl.Expression, ctx *hcl.EvalContext, field string) (int, error) {
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return 0, fmt.Errorf("%s: %s", field, diags.Error())
	}
	if val.IsNull() {
		return 0, nil
	}
	if val.Type() != cty.Number {
		return 0, fmt.Errorf("%s must be a number, got %s", field, val.Type().FriendlyName())
	}
	i64, _ := val.AsBigFloat().Int64()
	return int(i64), nil
}

	// evalStrList evaluates an expression expected to be a tuple/list of strings
func (r *resolver) evalStrList(expr hcl.Expression, self map[string]cty.Value, field string) ([]string, error) {
	if expr == nil {
		return nil, nil
	}
	ctx := r.ctxWith(self)
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: %s", field, diags.Error())
	}
	if val.IsNull() {
		return nil, nil
	}
	t := val.Type()
	if !t.IsTupleType() && !t.IsListType() {
		return nil, fmt.Errorf("%s must be a list of strings, got %s", field, t.FriendlyName())
	}
	var out []string
	for _, ev := range val.AsValueSlice() {
		switch ev.Type() {
		case cty.String:
			out = append(out, ev.AsString())
		case cty.Number, cty.Bool:
			out = append(out, fmt.Sprintf("%v", ctyToAny(ev)))
		default:
			return nil, fmt.Errorf("%s elements must be scalars, got %s", field, ev.Type().FriendlyName())
		}
	}
	return out, nil
}

// evalStr evaluates an expression expected to be a string (the `sudo`
// snippet). Nil expression ⇒ "".
func (r *resolver) evalStr(expr hcl.Expression, self map[string]cty.Value, field string) (string, error) {
	if expr == nil {
		return "", nil
	}
	ctx := r.ctxWith(self)
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return "", fmt.Errorf("%s: %s", field, diags.Error())
	}
	if val.IsNull() {
		return "", nil
	}
	if val.Type() != cty.String {
		return "", fmt.Errorf("%s must be a string, got %s", field, val.Type().FriendlyName())
	}
	return val.AsString(), nil
}

// ctxWith assembles the EvalContext from the resolver's running state plus
// the per-service `self` (omitted for non-service evaluation paths, if any
// are added in the future).
func (r *resolver) ctxWith(self map[string]cty.Value) *hcl.EvalContext {
	vars := map[string]cty.Value{}
	if len(r.serviceByTC) > 0 {
		toolchains := map[string]cty.Value{}
		for tc, services := range r.serviceByTC {
			toolchains[tc] = cty.ObjectVal(copyCtyMap(services))
		}
		vars["service"] = cty.ObjectVal(toolchains)
	}
	if self != nil {
		vars["self"] = cty.ObjectVal(copyCtyMap(self))
	}
	return &hcl.EvalContext{
		Variables: vars,
		Functions: r.functions(),
	}
}

func copyCtyMap(in map[string]cty.Value) map[string]cty.Value {
	out := make(map[string]cty.Value, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// mapValToCty projects a Go map[string]any (the evaluated form of vars or
// arguments) back into a cty.Value so subsequent stages can read it via
// self.vars.<key> / self.arguments[<key>].
// toStringMap stringifies an evaluated map (env values are always strings
// in the process environment; numbers/bools are coerced).
func toStringMap(m map[string]any) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

func mapValToCty(args map[string]any) cty.Value {
	if len(args) == 0 {
		return cty.EmptyObjectVal
	}
	out := make(map[string]cty.Value, len(args))
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = anyToCty(args[k])
	}
	return cty.ObjectVal(out)
}

// serviceDirOf returns the resolved per-invocation checkout dir of an
// already-built *Service (used when rebuilding the federation parent
// context from a running alpha's state). It's just the value the resolver
// already computed — no recomputation, no filesystem.
func serviceDirOf(s *Service) string {
	if s.Runtime == nil {
		return ""
	}
	return s.Runtime.Dir
}

// --- functions exposed in HCL expressions ---------------------------------

func (r *resolver) functions() map[string]function.Function {
	str := func(get func() string) function.Function {
		return function.New(&function.Spec{
			Type: function.StaticReturnType(cty.String),
			Impl: func(_ []cty.Value, _ cty.Type) (cty.Value, error) {
				v := get()
				if v == "" {
					return cty.NilVal, errors.New("fs:: function called but no invocation/service context")
				}
				return cty.StringVal(v), nil
			},
		})
	}
	return map[string]function.Function{
		// fs:: namespace — per-invocation filesystem coordinates and identity.
		"fs::tmp":  str(func() string { return r.inv.TmpDir }),   // generated files
		"fs::src":  str(func() string { return r.curCheckout }),  // this service's checkout
		"fs::bin":  str(func() string { return r.inv.BinDir() }), // build outputs (outside src)
		"fs::hash": r.fsHashFunc(),                               // instance identity (location)
		// cfg:: namespace — manifest identity (Alphasfile bytes + parent ctx).
		"cfg::hash": r.cfgHashFunc(),
		// src:: namespace — current service's source code identity.
		"src::hash": r.srcHashFunc(),

		"net::pickport": pickPortFunc(),
		"os::env":       osEnvFunc(),
	}
}

// osEnvFunc reads a host environment variable at evaluation time (in the
// zordon process, so it sees your shell env). os::env("NAME") errors if
// NAME is unset; os::env("NAME", "default") returns the default instead.
func osEnvFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "name", Type: cty.String}},
		VarParam: &function.Parameter{Name: "default", Type: cty.String,
			AllowNull: true},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			name := args[0].AsString()
			if v, ok := os.LookupEnv(name); ok {
				return cty.StringVal(v), nil
			}
			if len(args) > 1 {
				return cty.StringVal(args[1].AsString()), nil
			}
			return cty.NilVal, fmt.Errorf("os::env(%q): environment variable not set (pass a second arg for a default)", name)
		},
	})
}

// resolveDir turns a `dir` primary into an absolute path. ~ expands to
// $HOME; a relative path resolves against the Alphasfile's project root
// (so a committed example can say dir = "../.." portably). Empty stays
// empty (no dir primary).
func (r *resolver) resolveDir(dir string) string {
	if dir == "" {
		return ""
	}
	return resolveSrcDir(r.inv.ProjectRoot(), dir)
}

// fsHashFunc returns the short (16 hex chars) hash that identifies this
// alpha instance by its filesystem location (invocation dir + worktree).
// Stable per directory across runs and edits; unique across worktrees.
// Same token names the socket and tmp dir. Handy for collision-free
// hostnames in a shared reverse proxy: "myapp.${fs::hash()}.test".
func (r *resolver) fsHashFunc() function.Function {
	return function.New(&function.Spec{
		Type: function.StaticReturnType(cty.String),
		Impl: func(_ []cty.Value, _ cty.Type) (cty.Value, error) {
			if r.inv == nil || r.inv.FsHash == "" {
				return cty.NilVal, errors.New("fs::hash() called but no invocation configured")
			}
			return cty.StringVal(r.inv.FsHash), nil
		},
	})
}

// cfgHashFunc returns the short hash of the manifest (Alphasfile bytes +
// resolved parent context). Changes whenever any part of the manifest the
// alpha sees changes — what federation drift detection compares.
func (r *resolver) cfgHashFunc() function.Function {
	return function.New(&function.Spec{
		Type: function.StaticReturnType(cty.String),
		Impl: func(_ []cty.Value, _ cty.Type) (cty.Value, error) {
			if r.inv == nil || r.inv.CfgHash == "" {
				return cty.NilVal, errors.New("cfg::hash() called but no invocation configured")
			}
			return cty.StringVal(r.inv.CfgHash), nil
		},
	})
}

// srcHashFunc returns the short identity of the current service's source
// code. For a git-tracked checkout (dir/src primary, or a materialized git
// worktree) that's `git rev-parse --short HEAD`; otherwise it errors. Use
// it as a build cache key or a "code generation" stamp — pair with
// fs::hash() when you also need the location.
func (r *resolver) srcHashFunc() function.Function {
	return function.New(&function.Spec{
		Type: function.StaticReturnType(cty.String),
		Impl: func(_ []cty.Value, _ cty.Type) (cty.Value, error) {
			dir := r.curCheckout
			if dir == "" {
				return cty.NilVal, errors.New("src::hash(): no source primary for this service (use-only or no checkout)")
			}
			if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
				return cty.NilVal, fmt.Errorf("src::hash(): %s is not a git working tree (run zordon start once to materialize)", dir)
			}
			cmd := exec.Command("git", "-C", dir, "rev-parse", "--short=16", "HEAD")
			out, err := cmd.Output()
			if err != nil {
				return cty.NilVal, fmt.Errorf("src::hash(): git rev-parse HEAD in %s: %w", dir, err)
			}
			return cty.StringVal(strings.TrimSpace(string(out))), nil
		},
	})
}

func pickPortFunc() function.Function {
	return function.New(&function.Spec{
		Type: function.StaticReturnType(cty.Number),
		Impl: func(_ []cty.Value, _ cty.Type) (cty.Value, error) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return cty.NilVal, fmt.Errorf("net::pickport: %w", err)
			}
			addr := l.Addr().(*net.TCPAddr)
			_ = l.Close()
			return cty.NumberIntVal(int64(addr.Port)), nil
		},
	})
}

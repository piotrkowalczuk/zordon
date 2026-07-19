package alphasfile

import (
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/lifecycle"
	"github.com/piotrkowalczuk/zordon/internal/logfilter"
	"github.com/piotrkowalczuk/zordon/internal/zenv"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
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

// neverSentinel is the value the bare `never` keyword resolves to in
// `after = never`. It is not a barrier ref (those always contain '@'),
// so it's unambiguous: a provision whose only after entry is this
// sentinel is latent — registered but not auto-run at bringup.
const neverSentinel = "never"

// provisionRefMarker is the substring that distinguishes a *provision*
// barrier-attr object from a service/build/toolchain one. A cmd that is
// a bare traversal to service.<tc>.<svc>.runtime.provision.<name>
// resolves (via buildBarrierAttrs) to an object whose "success" attr is
// "<id>.runtime.provision.<name>@success" — we strip the @state suffix
// to recover the canonical target ID and treat the cmd as a CmdRef.
const provisionRefMarker = ".runtime.provision."

// provisionRefOf reports whether expr is a bare reference to another
// provision (its cty value is a provision barrier-attr object) and, if
// so, returns the canonical target provision ID. A normal shell snippet
// (string, possibly templated) returns ("", false, nil); only genuine
// evaluation errors propagate.
func (r *resolver) provisionRefOf(expr hcl.Expression, self map[string]cty.Value, dirs srcDirs) (string, bool, error) {
	if expr == nil {
		return "", false, nil
	}
	val, diags := expr.Value(r.ctxWith(self, dirs))
	if diags.HasErrors() {
		return "", false, fmt.Errorf("%s", diags.Error())
	}
	if val.IsNull() || !val.Type().IsObjectType() || !val.Type().HasAttribute("success") {
		return "", false, nil
	}
	succ := val.GetAttr("success")
	if succ.Type() != cty.String {
		return "", false, nil
	}
	s := succ.AsString()
	if !strings.Contains(s, provisionRefMarker) || !strings.HasSuffix(s, "@success") {
		return "", false, nil
	}
	return strings.TrimSuffix(s, "@success"), true, nil
}

// resolver walks the service DAG and produces the wire-stable *Alphasfile.
//
// Within each service, evaluation goes through a fixed sequence so that
// `self` can grow incrementally:
//
//  1. self_base   = { name, toolchain, dir }
//  2. eval Vars   → self.vars  = {...}
//  3. eval Files  → self.file.<name> = { path, body }
//  4. eval Args   → self.arguments = {...}
//  5. eval Probe  → readiness port
//
// Files come before arguments because the common pattern is to generate a
// config file and pass its path as a flag. Going the other way (file body
// referencing argument values) is uncommon and can be expressed via vars.
type resolver struct {
	path string
	// afDir is the absolute directory of the Alphasfile we're resolving.
	// Relative `src` and `dir` paths interpret against this — NOT the
	// invocation's working directory — so an Alphasfile means the same
	// thing whether the user runs zordon from project root or from a
	// subdir (walkUp finds the same Alphasfile either way).
	afDir string
	root  *rootBlock
	inv   *invocation.InvocationState
	// cfgHash is the manifest identity (sha8 of Alphasfile bytes + parent
	// ctx), threaded in by the caller because it depends on data the
	// resolver doesn't hold (raw bytes + serialized parent). It is what
	// cfg::hash() returns and what the resolved Alphasfile carries.
	cfgHash string

	// Already-evaluated services, keyed by toolchain then name. Re-projected
	// into the EvalContext under "service" before each new eval step. May be
	// pre-seeded with parent services for federation (flat namespace).
	serviceByTC map[string]map[string]cty.Value

	// toolchainCty is the projection of `toolchain { <lang> { ... } }`
	// declarations into cty: `toolchain.<lang>.ready` etc. resolve to
	// the canonical barrier-ref strings alpha then turns into real
	// *barrier.Barrier handles. Populated before any service eval so
	// service expressions can reference toolchain barriers.
	toolchainCty map[string]cty.Value

	// names already taken (parent or local); collision is an error.
	taken map[string]string // "tc/name" → origin ("parent" | "local")

	// testCfg carries the conformance-harness gating + log path the
	// test:: HCL functions need; zero value disables them.
	testCfg TestConfig

	resolvedServices []*Service
}

// Plan is a manifest's evaluation order: the prepared resolver, the topo-sorted
// producer nodes, and the toolchain pins — everything Compute needs to run the
// eval fold. Building a Plan is the structural pass; a dependency CYCLE surfaces
// HERE (topoSort), before Compute touches any effectful HCL function.
type Plan struct {
	r         *resolver
	order     []*node
	states    map[string]*svcState
	toolchain map[string]*ToolchainConfig
	parent    *ParentContext
}

// Plan prepares the manifest for evaluation: seeds the parent namespace,
// resolves toolchain pins, builds each service's static `self`, and topo-sorts
// the producer DAG. A real dependency cycle (A.vars→B.vars ∧ B.vars→A.vars) is
// the hard error here — caught before any effectful eval (net::pickport,
// src::hash) runs in Compute.
func (m *ManifestState) Plan(parent *ParentContext, cfgHash string, testCfg TestConfig) (*Plan, error) {
	name, root := m.name, m.root
	var seed map[string]map[string]cty.Value
	if parent != nil {
		seed = parent.byTC
	}
	if seed == nil {
		seed = map[string]map[string]cty.Value{}
	}
	// Compute the Alphasfile's own dir for relative path resolution.
	// Abs() is a no-op when the path is already absolute (the production
	// path, from cmd/zordon/federation.go); for tests passing a bare
	// filename it makes paths predictable against the test's working dir.
	absPath, _ := filepath.Abs(name)
	r := &resolver{
		path:        name,
		afDir:       filepath.Dir(absPath),
		root:        root,
		inv:         m.inv,
		cfgHash:     cfgHash,
		serviceByTC: seed,
		taken:       map[string]string{},
		testCfg:     testCfg,
	}

	parentKnown := map[string]struct{}{}
	for tc, byName := range seed {
		for name := range byName {
			r.taken[tc+"/"+name] = "parent"
			parentKnown[serviceID(tc, name)] = struct{}{}
		}
	}

	// Resolve toolchain block BEFORE services so each service's HCL can
	// reference `toolchain.<lang>.ready` etc. via the cty projection in
	// r.toolchainCty. Toolchain values themselves don't depend on
	// services, so ordering this first costs nothing.
	toolchain := map[string]*ToolchainConfig{}
	if parent != nil {
		maps.Copy(toolchain, parent.toolchain)
	}
	if root.Toolchain != nil {
		for lang, sub := range root.Toolchain.byLabel() {
			envMap, err := r.evalMap(sub.Env, nil, "toolchain."+lang+".env", srcDirs{})
			if err != nil {
				return nil, err
			}
			toolsMap, err := r.evalMap(sub.Tools, nil, "toolchain."+lang+".tools", srcDirs{})
			if err != nil {
				return nil, err
			}
			toolchain[lang] = &ToolchainConfig{
				Version: sub.Version,
				Tools:   toStringMap(toolsMap),
				Env:     toStringMap(envMap),
			}
		}
		// pkg pseudo-toolchain: standalone mise-backend CLIs (e.g.
		// aqua:ariga/atlas) that belong to no language. Stored under the
		// ToolchainPkg key with an empty Version; alpha installs each via
		// `mise install` and pools their bins behind
		// fs::toolchain::bin(toolchain.pkg).
		if root.Toolchain.Pkg != nil {
			toolsMap, err := r.evalMap(root.Toolchain.Pkg.Tools, nil, "toolchain.pkg.tools", srcDirs{})
			if err != nil {
				return nil, err
			}
			tools := toStringMap(toolsMap)
			if len(tools) == 0 {
				return nil, fmt.Errorf("toolchain.pkg: tools is required and must be non-empty (e.g. tools = { \"aqua:ariga/atlas\" = \"0.29.0\" })")
			}
			for ref, version := range tools {
				if strings.TrimSpace(version) == "" {
					return nil, fmt.Errorf("toolchain.pkg.tools[%q]: a version is required (e.g. %q = \"0.29.0\")", ref, ref)
				}
			}
			toolchain[ToolchainPkg] = &ToolchainConfig{Tools: tools}
		}
	}
	// Project pinned toolchains into cty so HCL expressions like
	// `toolchain.ruby.ready` resolve to the canonical barrier ref
	// `toolchain.ruby@ready`. Same self-discoverability rule as
	// service.runtime.* — the path mirrors the block nesting.
	r.toolchainCty = map[string]cty.Value{}
	for lang := range toolchain {
		r.toolchainCty[lang] = cty.ObjectVal(buildBarrierAttrs("toolchain."+lang, ToolchainBarrierStates))
	}

	// Pre-pass: for each service, validate identity, compute dir, build
	// the static slice of `self` (name/toolchain/dir + runtime/build
	// barrier-attr objects), and publish it to r.serviceByTC. This makes
	// barrier traversals like service.go.db.runtime.ready always
	// resolvable, regardless of evaluation order — they're constants
	// derived from labels, not expressions.
	states, err := r.prepareServices(root.Services)
	if err != nil {
		return nil, err
	}

	// Producer DAG over per-field (vars/arguments/env/file.<name>) nodes,
	// spanning ALL services. Cross-service references through any single
	// producer create a single edge between those two producer nodes —
	// not a whole-service edge. A.env→B.vars and B.env→A.vars are
	// independent and resolve cleanly; only a literal A.vars→B.vars
	// while B.vars→A.vars is a cycle, and that's a real bug.
	g, err := newGraph(root.Services, parentKnown)
	if err != nil {
		return nil, err
	}
	order, err := g.topoSort()
	if err != nil {
		return nil, err
	}
	return &Plan{r: r, order: order, states: states, toolchain: toolchain, parent: parent}, nil
}

// Compute runs the evaluation the Plan ordered: the producer fold (vars /
// arguments / env / file in dependency order), then per-service sinks
// (runtime/build/agent env, dotenv, print, sudo, provisions, readiness, the
// wire-stable Service), top-level dotenv + sysenv, the debugger-tools
// post-pass, and assembles the *Alphasfile. This is where the effectful HCL
// functions (net::pickport, src::hash, os::env) actually fire.
func (p *Plan) Compute() (*Alphasfile, error) {
	r := p.r
	root := r.root
	for _, n := range p.order {
		st := p.states[n.svcID]
		if err := r.evalProducerNode(n, st); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", n.svcID, producerLabel(n), err)
		}
	}

	// Sinks per service. Cross-service references in sinks reach into other
	// services' fully-evaluated producers via r.serviceByTC; sink-to-sink
	// references across services aren't supported, so sink order doesn't matter.
	for _, sb := range root.Services {
		sid := serviceID(sb.Toolchain, sb.Name)
		if err := r.finishService(p.states[sid]); err != nil {
			return nil, fmt.Errorf("%s: %w", sid, err)
		}
	}
	// File-level dotenv: top-level, no `self`; may use fs::/cfg:: funcs.
	gdot, err := r.evalStrOrList(root.Dotenv, nil, "dotenv", srcDirs{})
	if err != nil {
		return nil, err
	}
	// File-level inline env: same top-level scope as dotenv (no `self`).
	genvMap, err := r.evalMap(root.Env, nil, "env", srcDirs{})
	if err != nil {
		return nil, err
	}
	toolchain := p.toolchain
	if len(toolchain) == 0 {
		toolchain = nil
	}
	// SysEnv: parent's accumulated whitelist + this level's own.
	// Order-preserving union (see mergeSysEnv) so cfg::hash() is stable.
	localSysEnv, err := r.evalStrList(root.SysEnv, nil, "sysenv", srcDirs{})
	if err != nil {
		return nil, err
	}
	var parentSysEnv []string
	if p.parent != nil {
		parentSysEnv = p.parent.sysenv
	}
	sysEnv := mergeSysEnv(parentSysEnv, localSysEnv)
	if len(sysEnv) == 0 {
		sysEnv = nil
	}
	// Post-pass: services with `debugger.enabled = true` need dlv and
	// mcp-dap-server in the matching toolchain's tool world. We add
	// them after per-service eval so user-pinned versions (already in
	// toolchain.tools) win over the macro's defaults.
	if err := r.injectDebuggerTools(toolchain); err != nil {
		return nil, err
	}
	if err := validateProvisionArgRefs(r.resolvedServices); err != nil {
		return nil, err
	}
	return &Alphasfile{
		CfgHash:   r.cfgHash,
		Dotenv:    gdot,
		Env:       toStringMap(genvMap),
		Services:  r.resolvedServices,
		Toolchain: toolchain,
		SysEnv:    sysEnv,
	}, nil
}

// validateProvisionArgRefs rejects a cmd-ref whose target provision declares
// arguments: the invoker would inherit unsubstituted placeholders (a target's
// arguments are bound only at its own invoke). Argument-bearing provisions are
// invoked at runtime, never used as cmd-ref templates. Limited to provisions in
// this Alphasfile level (cross-federation refs aren't checked here).
func validateProvisionArgRefs(services []*Service) error {
	byID := map[string]*ProvisionStep{}
	for _, s := range services {
		if s == nil || s.Runtime == nil {
			continue
		}
		sid := serviceID(s.Toolchain, s.Name())
		for _, p := range s.Runtime.Provision {
			byID[sid+".runtime.provision."+p.Name] = p
		}
	}
	for _, s := range services {
		if s == nil || s.Runtime == nil {
			continue
		}
		for _, p := range s.Runtime.Provision {
			if p.CmdRef == "" {
				continue
			}
			if tgt, ok := byID[p.CmdRef]; ok && len(tgt.Arguments) > 0 {
				return fmt.Errorf("provision %q: cmd references %q which declares arguments; invoke an argument-bearing provision at runtime, not via cmd-ref", p.Name, p.CmdRef)
			}
		}
	}
	return nil
}

const (
	debuggerToolDlv = "github.com/go-delve/delve/cmd/dlv"
	debuggerToolMCP = "github.com/go-delve/mcp-dap-server"
)

func (r *resolver) injectDebuggerTools(toolchain map[string]*ToolchainConfig) error {
	needs := false
	for _, svc := range r.resolvedServices {
		if svc.Debugger != nil && svc.Debugger.Enabled && svc.Toolchain == ToolchainGo {
			needs = true
			break
		}
	}
	if !needs {
		return nil
	}
	tc, ok := toolchain[ToolchainGo]
	if !ok {
		return fmt.Errorf("debugger { enabled = true } requires a `toolchain { go { version = … } }` block (so dlv + mcp-dap-server can be installed into the pinned Go's tool world)")
	}
	if tc.Tools == nil {
		tc.Tools = map[string]string{}
	}
	// Explicit user pins win.
	if _, pinned := tc.Tools[debuggerToolDlv]; !pinned {
		tc.Tools[debuggerToolDlv] = "latest"
	}
	if _, pinned := tc.Tools[debuggerToolMCP]; !pinned {
		tc.Tools[debuggerToolMCP] = "latest"
	}
	return nil
}

// svcState is the per-service evaluation scratch space. Built once
// per service in prepareServices, mutated by per-producer eval, and
// consumed by finishService when assembling the wire-stable Service.
type svcState struct {
	sb       *serviceBlock
	id       string // "service.<tc>.<name>"
	dir      string
	self     map[string]cty.Value // grows: name/toolchain/dir/runtime/build → +vars/+env/+args/+file
	files    []*File
	fileVals map[string]cty.Value
	vars     map[string]any
	args     map[string]map[string]any
	envMap   map[string]any
	dirs     srcDirs // source locations the fs:: functions return for this service
	srcPath  string  // evaluated src{path} (local checkout); empty when no src{path}
}

// srcDirs carries a service's source locations for the fs:: functions during
// eval: root is the checkout root (fs::src ≈ src.path), exe is the exe-anchored
// working dir (fs::exe ≈ src.path + src.exe = self.dir). Zero value = file scope
// (no service): both "", so fs::src/fs::exe error cleanly.
type srcDirs struct {
	root   string // checkout root        → fs::src()
	exe    string // <checkout>/<exe>      → fs::exe()
	etc    string // <StateDir>/etc/<svc>  → fs::etc()
	vardir string // <StateDir>/var/<svc>  → fs::var()
}

// prepareServices initializes one svcState per service: validates
// identity, computes dir, builds the static slice of `self` (name,
// toolchain, dir, runtime/build barrier-attr objects), and publishes
// that partial cty value to r.serviceByTC so cross-service barrier
// traversals are resolvable before any expression runs. Producer
// values (vars/arguments/env/file) are written in later; the partial
// is re-projected to r.serviceByTC after each per-producer eval.
func (r *resolver) prepareServices(services []*serviceBlock) (map[string]*svcState, error) {
	out := make(map[string]*svcState, len(services))
	for _, sb := range services {
		// A local service with the same name as a federation-parent service
		// COMPLETELY overrides it: the child definition replaces the
		// parent's in this level's namespace (and runs in this level's
		// alpha). Two local blocks with the same name in one file is still
		// a real mistake.
		if origin := r.taken[sb.Toolchain+"/"+sb.Name]; origin == "local" {
			return nil, fmt.Errorf("duplicate service %s.%s in this Alphasfile", sb.Toolchain, sb.Name)
		}
		r.taken[sb.Toolchain+"/"+sb.Name] = "local"

		// dir = where this service's code lives for this invocation.
		//
		//   src-only, used IN PLACE (your live working tree; zordon
		//     never copies or git-workspaces it) when either:
		//       - main workspace (the default; "main" just means "use
		//         src directly"), OR
		//       - named workspace that did NOT pick this service at
		//         `workspace create` (we share the anchor Alphasfile's
		//         tree so provision shells with relative paths like
		//         `./plik.sql` resolve against the real source).
		//   anything else (named workspace owning the service, or any
		//     git primary) → a per-invocation checkout alpha
		//     materializes via git worktree add.
		//
		// inv.OwnsService is the FS-derived ownership predicate
		// (presence of <wtdir>/src/<svc>/.git); see invocation.build.
		// Pure within this function — the FS scan happens once at
		// invocation construction.
		checkout := ""
		var gitURL, srcPath, srcExeFromSB string
		if sb.Git != nil {
			gitURL = sb.Git.URL
		}
		if sb.Src != nil {
			// Empty srcDirs: src{path} is the input that *defines* the
			// checkout, so fs::src()/fs::exe() aren't available to it yet
			// (they'd error cleanly). os:: and cross-service refs work.
			p, err := r.evalStr(sb.Src.Path, nil, "src.path", srcDirs{})
			if err != nil {
				return nil, fmt.Errorf("service %q: %w", sb.Name, err)
			}
			srcPath = p
			srcExeFromSB = sb.Src.Exe
		}
		switch {
		case srcPath != "" && gitURL == "" && !r.inv.OwnsService(sb.Name):
			checkout = r.resolveDir(srcPath)
		case gitURL != "" || srcPath != "":
			checkout = r.inv.CheckoutPath(sb.Name)
		}
		// Exe-anchor: the canonical "service working directory" is
		// `<checkout>/<exe>`, computed once via zfs.ServiceCwd and used
		// uniformly for build cwd, runtime cwd, provision cwd, and
		// `fs::src()`. checkout == "" (use-only install) keeps dir == "".
		dir := zfs.ServiceCwd(checkout, srcExeFromSB)

		// self_base — known from labels + primary, before any eval.
		//
		// Status barriers and provisions hang off `self.runtime.*` so
		// the reference path mirrors the HCL block nesting: a user
		// staring at `runtime { provision "x" { ... } }` can guess the
		// canonical path `self.runtime.provision.x.<status>` without
		// consulting docs. The rule: traversal shape ≡ schema shape.
		// Resolved scalars/maps that belong to the service entity
		// itself (name, toolchain, dir, vars, arguments, file) stay at
		// self.* because that's where they live in HCL too.
		sid := serviceID(sb.Toolchain, sb.Name)
		self := map[string]cty.Value{
			"name":      cty.StringVal(sb.Name),
			"toolchain": cty.StringVal(sb.Toolchain),
			"dir":       cty.StringVal(dir),
		}
		runtimeAttrs := buildBarrierAttrs(sid+".runtime", ServiceBarrierStates)
		if sb.Runtime != nil && len(sb.Runtime.Provision) > 0 {
			provObjs := make(map[string]cty.Value, len(sb.Runtime.Provision))
			for _, pb := range sb.Runtime.Provision {
				provID := sid + ".runtime.provision." + pb.Name
				provObjs[pb.Name] = cty.ObjectVal(buildBarrierAttrs(provID, ProvisionBarrierStates))
			}
			runtimeAttrs["provision"] = cty.ObjectVal(provObjs)
		}
		self["runtime"] = cty.ObjectVal(runtimeAttrs)
		// `self.build.<state>` mirrors the build block. Always exposed
		// (even when no explicit build cmd) — the lifecycle still fires
		// success the moment prepare finishes, so cross-service
		// `after = [...build.success]` resolves uniformly.
		self["build"] = cty.ObjectVal(buildBarrierAttrs(sid+".build", BuildBarrierStates))
		// Empty defaults for not-yet-evaluated producers so a service
		// referencing another service with no vars/file block still gets
		// a resolvable (empty) value rather than a missing-attr error.
		self["vars"] = cty.EmptyObjectVal
		self["arguments"] = cty.EmptyObjectVal
		self["env"] = cty.EmptyObjectVal
		self["file"] = cty.EmptyObjectVal

		var etcDir, varDir string
		if r.inv != nil {
			etcDir = filepath.Join(r.inv.StateDir, "etc", sb.Name)
			varDir = filepath.Join(r.inv.StateDir, "var", sb.Name)
		}
		st := &svcState{
			sb:       sb,
			id:       sid,
			dir:      dir,
			self:     self,
			fileVals: map[string]cty.Value{},
			dirs:     srcDirs{root: checkout, exe: dir, etc: etcDir, vardir: varDir},
			srcPath:  srcPath,
		}
		out[sid] = st
		r.publishSelf(st)
	}
	return out, nil
}

// publishSelf re-projects st.self into r.serviceByTC so cross-service
// traversals see the latest partial. Called after each per-producer
// eval (and once from prepareServices for the static slice).
func (r *resolver) publishSelf(st *svcState) {
	if r.serviceByTC[st.sb.Toolchain] == nil {
		r.serviceByTC[st.sb.Toolchain] = map[string]cty.Value{}
	}
	r.serviceByTC[st.sb.Toolchain][st.sb.Name] = cty.ObjectVal(st.self)
}

// evalProducerNode runs one node from the cross-service DAG: a single
// service's vars, arguments, env, or one file. Updates st.self in
// place and re-publishes to r.serviceByTC so downstream nodes see the
// new value.
func (r *resolver) evalProducerNode(n *node, st *svcState) error {
	switch n.kind {
	case kindVars:
		if st.sb.Vars == nil {
			return nil
		}
		v, err := r.evalMap(st.sb.Vars, st.self, "vars", st.dirs)
		if err != nil {
			return err
		}
		st.vars = v
		st.self["vars"] = mapValToCty(v)
	case kindArguments:
		if st.sb.Arguments == nil || st.sb.Arguments.Values == nil {
			return nil
		}
		groups, err := r.evalArgGroups(st.sb.Arguments.Values, st.self, st.dirs, st.sb.Name)
		if err != nil {
			return err
		}
		st.args = groups
		st.self["arguments"] = argsSelfCty(groups)
	case kindEnv:
		if st.sb.Env == nil {
			return nil
		}
		e, err := r.evalMap(st.sb.Env, st.self, "env", st.dirs)
		if err != nil {
			return err
		}
		st.envMap = e
		st.self["env"] = mapValToCty(e)
	case kindFile:
		var fb *fileBlock
		for _, x := range st.sb.Files {
			if x.Name == n.name {
				fb = x
				break
			}
		}
		if fb == nil {
			return fmt.Errorf("file %q: block lost between parse and eval", n.name)
		}
		path, body, err := evalFileExpr(fb, r.ctxWith(st.self, st.dirs))
		if err != nil {
			return err
		}
		st.files = append(st.files, &File{Name: fb.Name, Path: path, Body: body})
		st.fileVals[fb.Name] = cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal(fb.Name),
			"path": cty.StringVal(path),
			"body": cty.StringVal(body),
		})
		st.self["file"] = cty.ObjectVal(st.fileVals)
	}
	r.publishSelf(st)
	return nil
}

// producerLabel returns the user-facing field name of a producer node
// for error messages (e.g., "vars" or "file.config").
func producerLabel(n *node) string {
	if n.kind == kindFile {
		return "file." + n.name
	}
	return n.kind.String()
}

// finishService evaluates a service's sinks (runtime.cmd, build.cmd,
// build/runtime/agent env, dotenv, print, sudo, provisions, readiness)
// against the fully-populated st.self and assembles the wire-stable
// Service. Cross-service references reach producers via r.serviceByTC,
// which by now holds every service's complete evaluated form.
func (r *resolver) finishService(st *svcState) error {
	sb := st.sb
	self := st.self
	dirs := st.dirs

	// Sinks: consume the fully-populated self; order among them is
	// irrelevant since nothing reads them back.
	var (
		command []string
		err     error
	)
	argOpts, err := resolveArgOptions(sb)
	if err != nil {
		return err
	}
	if sb.Runtime != nil && sb.Runtime.Cmd != nil {
		command, err = r.evalCmd(sb.Runtime.Cmd, self, dirs, st.args, argOpts, sb.Toolchain)
		if err != nil {
			return err
		}
	}
	var buildCmd []string
	if sb.Build != nil && sb.Build.Cmd != nil {
		buildCmd, err = r.evalStrList(sb.Build.Cmd, self, "build.cmd", dirs)
		if err != nil {
			return err
		}
	}
	phaseEnv := func(expr hcl.Expression, label string) (map[string]string, error) {
		if expr == nil {
			return nil, nil
		}
		m, e := r.evalMap(expr, self, label, dirs)
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
	var runtimeAfter []string
	if sb.Runtime != nil {
		if runEnv, err = phaseEnv(sb.Runtime.Env, "runtime.env"); err != nil {
			return err
		}
		runtimeAfter, err = r.evalStrList(sb.Runtime.After, self, "runtime.after", dirs)
		if err != nil {
			return err
		}
	}
	if sb.Agent != nil {
		if agentEnv, err = phaseEnv(sb.Agent.Env, "agent.env"); err != nil {
			return err
		}
	}
	dotenv, err := r.evalStrOrList(sb.Dotenv, self, "dotenv", dirs)
	if err != nil {
		return err
	}
	printLine, err := r.evalStr(sb.Print, self, "print", dirs)
	if err != nil {
		return err
	}
	// sudo hooks (may reference everything resolved so far).
	var sudo []*SudoStep
	for _, sblk := range sb.Sudo {
		check, err := r.evalStr(sblk.Check, self, "sudo."+sblk.Name+".check", dirs)
		if err != nil {
			return err
		}
		apply, err := r.evalStr(sblk.Apply, self, "sudo."+sblk.Name+".apply", dirs)
		if err != nil {
			return err
		}
		verify, err := r.evalStr(sblk.Verify, self, "sudo."+sblk.Name+".verify", dirs)
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

			// Declared arguments are parsed first: their names define the
			// placeholder bindings the snippets evaluate against. `self` is
			// augmented with this provision's own arguments only, so a
			// reference to another provision's `.arguments` errors out.
			pargs, err := r.evalProvisionArgs(pb, self, dirs, label)
			if err != nil {
				return err
			}
			argNames := make([]string, len(pargs))
			for i, a := range pargs {
				argNames[i] = a.Name
			}
			selfP := provisionSelfWithArgs(self, pb.Name, argNames)

			check, err := r.evalStr(pb.Check, selfP, label+".check", dirs)
			if err != nil {
				return err
			}
			// cmd is either an inline shell snippet (string) or a bare
			// reference to another (latent) provision used as a template.
			cmdRef, isRef, err := r.provisionRefOf(pb.Cmd, selfP, dirs)
			if err != nil {
				return fmt.Errorf("%s.cmd: %w", label, err)
			}
			var cmd string
			if isRef {
				if cmdRef == st.id+".runtime.provision."+pb.Name {
					return fmt.Errorf("%s.cmd: provision cannot reference itself", label)
				}
			} else {
				cmd, err = r.evalStr(pb.Cmd, selfP, label+".cmd", dirs)
				if err != nil {
					return err
				}
			}
			verify, err := r.evalStr(pb.Verify, selfP, label+".verify", dirs)
			if err != nil {
				return err
			}
			// clean is the provision's own teardown snippet (run by `zordon
			// clean`), interpolated in the same scope as cmd/verify. Unlike
			// check/verify it is allowed alongside a cmd-ref: it undoes what
			// this invoker did, not what the referenced template owns.
			clean, err := r.evalStr(pb.Clean, selfP, label+".clean", dirs)
			if err != nil {
				return err
			}
			if isRef && (strings.TrimSpace(check) != "" || strings.TrimSpace(verify) != "") {
				return fmt.Errorf("%s: check/verify are not allowed when cmd references another provision (the template owns them)", label)
			}
			if len(pargs) > 0 && isRef {
				return fmt.Errorf("%s: `argument` blocks are not allowed when cmd references another provision", label)
			}
			var penv map[string]string
			if pb.Env != nil {
				m, err := r.evalMap(pb.Env, self, label+".env", dirs)
				if err != nil {
					return err
				}
				penv = toStringMap(m)
			}
			// `after` accepts the bare keyword `never` (a latent
			// provision: registered but not auto-run) OR a list of
			// barrier refs — never both.
			afterList, err := r.evalStrOrList(pb.After, self, label+".after", dirs)
			if err != nil {
				return err
			}
			latent := false
			afterClean := afterList[:0]
			for _, a := range afterList {
				if a == neverSentinel {
					latent = true
					continue
				}
				afterClean = append(afterClean, a)
			}
			if latent && len(afterClean) > 0 {
				return fmt.Errorf("%s.after: `never` cannot be combined with other refs", label)
			}
			if latent && pb.Detached {
				return fmt.Errorf("%s: `detached` is meaningless on a latent (`after = never`) provision", label)
			}
			if len(pargs) > 0 && !latent {
				return fmt.Errorf("%s: a provision with `argument` blocks must be latent (`after = never`) — its placeholders are only substituted at invoke", label)
			}
			provisions = append(provisions, &ProvisionStep{
				Name:        pb.Name,
				Description: pb.Description,
				Arguments:   pargs,
				Check:       check,
				Cmd:         cmd,
				Verify:      verify,
				Clean:       clean,
				Env:         penv,
				After:       afterClean,
				Detached:    pb.Detached,
				Latent:      latent,
				CmdRef:      cmdRef,
			})
		}
	}

	// Stage 7: readiness action (may reference everything in self).
	var (
		probePort    int
		probeExecCmd []string
		probeExecEnv map[string]string
	)
	if sb.Readiness != nil {
		ctx := r.ctxWith(self, dirs)
		var (
			expr  hcl.Expression
			field string
		)
		switch {
		case sb.Readiness.HTTP != nil:
			expr, field = sb.Readiness.HTTP.Port, "readiness.http.port"
		case sb.Readiness.TCP != nil:
			expr, field = sb.Readiness.TCP.Port, "readiness.tcp.port"
		}

		if expr != nil {
			probePort, err = evalIntExpr(expr, ctx, field)
			if err != nil {
				return err
			}
		}
		if sb.Readiness.Exec != nil {
			probeExecCmd, err = r.evalStrList(sb.Readiness.Exec.Command, self, "readiness.exec.command", dirs)
			if err != nil {
				return err
			}
			if sb.Readiness.Exec.Env != nil {
				m, err := r.evalMap(sb.Readiness.Exec.Env, self, "readiness.exec.env", dirs)
				if err != nil {
					return err
				}
				probeExecEnv = toStringMap(m)
			}
		}
	}

	// Build the wire-stable Service and runtime config.
	rt := &RuntimeConfig{
		Name:      sb.Name,
		Color:     sb.Color,
		Options:   argOpts,
		Vars:      st.vars,
		Arguments: st.args,
		Env:       toStringMap(st.envMap),
		BuildEnv:  buildEnv,
		RunEnv:    runEnv,
		AgentEnv:  agentEnv,
		Dotenv:    dotenv,
		Command:   command,
		After:     runtimeAfter,
		Sudo:      sudo,
		Provision: provisions,
		Files:     st.files,
		Dir:       st.dir,
		Checkout:  st.dirs.root,
		BinDir:    r.inv.BinDir(),
		EtcDir:    st.dirs.etc,
		VarDir:    st.dirs.vardir,
		Print:     printLine,
	}
	// Resolve per-toolchain defaults so the wire-stable Service is fully
	// populated — alpha can read rt.Log.TTY without knowing what
	// "ruby" means.
	rt.Log = &LogConfig{}
	if sb.Log != nil {
		rt.Log.Format = sb.Log.Format
		rt.Log.Filter = sb.Log.Filter
		rt.Log.TTY = sb.Log.TTY
		if err := logfilter.Validate(rt.Log.Filter); err != nil {
			return fmt.Errorf("log filter: %w", err)
		}
	}
	if rt.Log.TTY == nil {
		def := toolchainDefaultsFor[sb.Toolchain].TTY
		rt.Log.TTY = &def
	}
	if sb.Readiness != nil {
		p, err := compileProbe(sb.Readiness, probePort, probeExecCmd, probeExecEnv)
		if err != nil {
			return fmt.Errorf("readiness: %w", err)
		}
		rt.Readiness = p
	}

	switch sb.Toolchain {
	case ToolchainGo, ToolchainRust, ToolchainRuby, ToolchainNode, ToolchainPkg:
	default:
		return fmt.Errorf("unknown toolchain %q (want go|rust|ruby|nodejs|pkg)", sb.Toolchain)
	}

	// `package` is polymorphic — a string (go) or a string/object (pkg).
	// Evaluate it once; nil means absent.
	pkgC, err := r.evalPackage(sb.Package, self, dirs, "package")
	if err != nil {
		return err
	}

	// pkg: a mise-materialized native package with no source build —
	// resolved entirely here, sharing none of the src/git/crate plumbing.
	if sb.Toolchain == ToolchainPkg {
		if sb.Src != nil || sb.Git != nil || sb.Crate != nil || sb.Bin != "" || len(sb.Features) > 0 {
			return fmt.Errorf("service %q: pkg has no source build — drop src/git/crate/bin/features", sb.Name)
		}
		if sb.Debugger != nil && sb.Debugger.Enabled {
			return fmt.Errorf("service %q: debugger {} is not supported on pkg services", sb.Name)
		}
		if len(command) == 0 {
			return fmt.Errorf("service %q: pkg requires runtime { cmd = [...] } (no entrypoint inference)", sb.Name)
		}
		if pkgC == nil {
			return fmt.Errorf("service %q: pkg requires a `package` coordinate", sb.Name)
		}
		pname, pversion, perr := pkgC.miseSpec()
		if perr != nil {
			return fmt.Errorf("service %q: %w", sb.Name, perr)
		}
		svc := &Service{
			Toolchain: ToolchainPkg,
			Runtime:   rt,
			Pkg:       &PkgSpec{Name: pname, Version: pversion},
		}
		r.resolvedServices = append(r.resolvedServices, svc)
		return nil
	}
	// Use-only: the install coordinate comes from `crate {}` (rust) or
	// `package` (go). Cross-toolchain mixing is a mistake.
	var install, instVersion, instIndex, instRegistry string
	var instGit, instBranch, instTag, instRev string
	var features []string
	switch sb.Toolchain {
	case ToolchainGo:
		if sb.Crate != nil {
			return fmt.Errorf("service %q is go but declares a `crate {}` block (rust-only)", sb.Name)
		}
		if len(sb.Features) > 0 {
			return fmt.Errorf("service %q is go but declares `features` (rust-only)", sb.Name)
		}
		if pkgC != nil {
			if pkgC.isObj {
				return fmt.Errorf("service %q: go `package` must be a string (got an object)", sb.Name)
			}
			install = pkgC.raw
		}
	case ToolchainRust:
		if pkgC != nil {
			return fmt.Errorf("service %q is rust but declares a top-level `package` field (go-only)", sb.Name)
		}
		features = sb.Features
		if sb.Crate != nil {
			install = strings.TrimSpace(sb.Crate.Name)
			if install == "" {
				return fmt.Errorf("service %q: crate { name } is required", sb.Name)
			}
			instVersion = strings.TrimSpace(sb.Crate.Version)
			instIndex = strings.TrimSpace(sb.Crate.Index)
			instRegistry = strings.TrimSpace(sb.Crate.Registry)
			instGit = strings.TrimSpace(sb.Crate.Git)
			instBranch = strings.TrimSpace(sb.Crate.Branch)
			instTag = strings.TrimSpace(sb.Crate.Tag)
			instRev = strings.TrimSpace(sb.Crate.Rev)
			// cargo install rejects --version with --git, and the three
			// git refs are themselves mutually exclusive.
			if instGit == "" && (instBranch != "" || instTag != "" || instRev != "") {
				return fmt.Errorf("service %q: crate { branch/tag/rev } require crate { git }", sb.Name)
			}
			if instVersion != "" && instGit != "" {
				return fmt.Errorf("service %q: crate { version } and crate { git } are mutually exclusive", sb.Name)
			}
			refs := 0
			for _, s := range []string{instBranch, instTag, instRev} {
				if s != "" {
					refs++
				}
			}
			if refs > 1 {
				return fmt.Errorf("service %q: crate { branch/tag/rev } are mutually exclusive", sb.Name)
			}
		}
	default:
		if pkgC != nil || sb.Crate != nil {
			return fmt.Errorf("service %q: %s has no use-only mode", sb.Name, sb.Toolchain)
		}
		if len(sb.Features) > 0 {
			return fmt.Errorf("service %q: features is rust-only", sb.Name)
		}
	}

	// Modes: use-only (install), remote-workspace (git block), or
	// local-workspace (src{path}). git+src{exe} (no path) is allowed:
	// src carries only the build subdir inside the cloned remote.
	var srcGitURL, srcLocalPath, srcBranch, srcTag, srcRev, srcExe string
	if sb.Git != nil {
		srcGitURL = strings.TrimSpace(sb.Git.URL)
		srcBranch = sb.Git.Branch
		srcTag = sb.Git.Tag
		srcRev = sb.Git.Rev
	}
	if sb.Src != nil {
		srcLocalPath = st.srcPath
		srcExe = sb.Src.Exe
	}
	switch {
	case sb.Src != nil && sb.Git != nil && srcLocalPath != "":
		return fmt.Errorf("service %q: src{path} and git{} are mutually exclusive (src can only carry exe alongside git)", sb.Name)
	case sb.Src != nil && sb.Git == nil && srcLocalPath == "":
		return fmt.Errorf("service %q: src{} without path requires a sibling git{} block", sb.Name)
	case install != "" && (srcGitURL != "" || srcLocalPath != "" || (sb.Src != nil && srcExe != "")):
		return fmt.Errorf("service %q: use-only (crate/package) cannot coexist with src/git", sb.Name)
	case install == "" && srcGitURL == "" && srcLocalPath == "":
		field := "package"
		if sb.Toolchain == ToolchainRust {
			field = "crate {}"
		}
		return fmt.Errorf("service %q has no source: declare src {}, git {}, or %s", sb.Name, field)
	}

	var workspace *Workspace
	if sb.Workspace != nil && len(sb.Workspace.Sparse) > 0 {
		// Sparse cone paths are relative to the primary repo root (what
		// `git sparse-checkout set` expects) — pass verbatim, not joined
		// to an absolute Alphasfile path.
		workspace = &Workspace{Sparse: cleanSparse(sb.Workspace.Sparse)}
	}

	var dbg *DebuggerConfig
	var agent *ServiceAgent
	if sb.Debugger != nil && sb.Debugger.Enabled {
		d, a, err := r.evalDebugger(sb, self, command, dirs)
		if err != nil {
			return err
		}
		dbg = d
		agent = a
	}

	// For use-only-from-git (rust crate { git, branch/tag/rev }) the git
	// fields ride along in Package.Git/Branch/Tag/Rev — the alpha-side
	// command builder reads them when Install != "". For workspace mode
	// they hold the clone coordinates instead.
	pkgGit, pkgBranch, pkgTag, pkgRev := srcGitURL, srcBranch, srcTag, srcRev
	if install != "" && instGit != "" {
		pkgGit, pkgBranch, pkgTag, pkgRev = instGit, instBranch, instTag, instRev
	}
	svc := &Service{
		Toolchain: sb.Toolchain,
		Runtime:   rt,
		Package: &Package{
			Toolchain: sb.Toolchain,
			Git:       pkgGit,
			Src:       r.resolveDir(srcLocalPath),
			Branch:    pkgBranch,
			Tag:       pkgTag,
			Rev:       pkgRev,
			Install:   install,
			Version:   instVersion,
			Index:     instIndex,
			Registry:  instRegistry,
			Features:  features,
			Exe:       srcExe,
			Bin:       sb.Bin,
			BuildCmd:  buildCmd,
			Cmd:       strings.Join(command, " "),
			Workspace: workspace,
			// In-place: src-only AND either main workspace, or a named
			// workspace that did not pick this service (no per-workspace
			// checkout under <wtdir>/src/<svc>). Alpha builds/runs from
			// src as-is — no git worktree add, no HEAD reset.
			InPlace: srcLocalPath != "" && !r.inv.OwnsService(sb.Name),
		},
		Debugger: dbg,
		Agent:    agent,
	}
	r.resolvedServices = append(r.resolvedServices, svc)
	return nil
}

func (r *resolver) evalDebugger(sb *serviceBlock, self map[string]cty.Value, command []string, dirs srcDirs) (*DebuggerConfig, *ServiceAgent, error) {
	db := sb.Debugger
	if sb.Toolchain != ToolchainGo {
		return nil, nil, fmt.Errorf("service %q: debugger {} is currently only supported on service \"go\" (got %q)", sb.Name, sb.Toolchain)
	}
	mcp := true
	if db.MCP != nil {
		mcp = *db.MCP
	}
	wrap := true
	if db.WrapRuntime != nil {
		wrap = *db.WrapRuntime
	}
	waitForClient := false
	if db.WaitForClient != nil {
		waitForClient = *db.WaitForClient
	}
	logFlag := false
	if db.Log != nil {
		logFlag = *db.Log
	}
	if wrap && len(command) > 0 {
		return nil, nil, fmt.Errorf("service %q: debugger.enabled = true wraps runtime itself (`dlv exec <binary> -- <args>`), which is incompatible with an explicit `runtime.cmd`. Use `arguments { values = {…} }` for flags, or set `debugger.wrap_runtime = false` to keep your own cmd", sb.Name)
	}
	port, err := r.evalDebuggerPort(db.Port, self, dirs)
	if err != nil {
		return nil, nil, fmt.Errorf("service %q: debugger.port: %w", sb.Name, err)
	}
	cfg := &DebuggerConfig{
		Enabled:       true,
		Port:          port,
		WaitForClient: waitForClient,
		Log:           logFlag,
		MCP:           mcp,
		WrapRuntime:   wrap,
	}
	var agent *ServiceAgent
	if mcp {
		agent = &ServiceAgent{
			MCP: map[string]*AgentMCPFeature{
				"debug": {
					Name:    "debug",
					Bridge:  "dap",
					Address: fmt.Sprintf("127.0.0.1:%d", port),
				},
			},
		}
	}
	return cfg, agent, nil
}

// evalDebuggerPort resolves debugger.port to an int, accepting either a
// numeric literal (`port = 2345`) or a string returned by an HCL helper
// (`port = os::env("DLV_PORT", "2345")`). When the expression is nil,
// it falls back to $DLV_PORT then 2345 — matching the default we'd
// write inline if the macro didn't exist.
func (r *resolver) evalDebuggerPort(expr hcl.Expression, self map[string]cty.Value, dirs srcDirs) (int, error) {
	if expr != nil {
		val, diags := expr.Value(r.ctxWith(self, dirs))
		if diags.HasErrors() {
			return 0, fmt.Errorf("%s", diags.Error())
		}
		// `port,optional` gives a non-nil placeholder when omitted (null/dynamic).
		if !val.IsNull() && val.Type() != cty.DynamicPseudoType {
			switch val.Type() {
			case cty.Number:
				i64, _ := val.AsBigFloat().Int64()
				return int(i64), nil
			case cty.String:
				return parsePort(val.AsString())
			}
			return 0, fmt.Errorf("must be a number or numeric string, got %s", val.Type().FriendlyName())
		}
	}
	return pickFreePort()
}

func parsePort(s string) (int, error) {
	n := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, fmt.Errorf("not a valid port number: %q", s)
	}
	if n <= 0 || n > 65535 {
		return 0, fmt.Errorf("port %d out of range [1, 65535]", n)
	}
	return n, nil
}

// evalMap evaluates an HCL expression that should yield a map/object and
// returns its members as a Go map (for embedding in RuntimeConfig) plus
// nil-safety. Used for both `vars` and `arguments`.
func (r *resolver) evalMap(expr hcl.Expression, self map[string]cty.Value, field string, dirs srcDirs) (map[string]any, error) {
	if expr == nil {
		return nil, nil
	}
	ctx := r.ctxWith(self, dirs)
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

// evalArgGroups resolves `arguments { values = {…} }` into named groups
// (group name → flag→value map). Each top-level entry must be an object
// (a group); each flag value must be a scalar. This is the two-level shape
// Flags() and tpl::render::flags render from.
func (r *resolver) evalArgGroups(expr hcl.Expression, self map[string]cty.Value, dirs srcDirs, svc string) (map[string]map[string]any, error) {
	ctx := r.ctxWith(self, dirs)
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("arguments.values: %s", diags.Error())
	}
	if val.IsNull() {
		return nil, nil
	}
	if t := val.Type(); !t.IsObjectType() && !t.IsMapType() {
		return nil, fmt.Errorf("service %q: arguments.values must be an object of named groups, got %s", svc, t.FriendlyName())
	}
	groups := map[string]map[string]any{}
	for name, gv := range val.AsValueMap() {
		gt := gv.Type()
		if gv.IsNull() || (!gt.IsObjectType() && !gt.IsMapType()) {
			return nil, fmt.Errorf("service %q: arguments.values.%s must be a group (object of flags), got %s", svc, name, gt.FriendlyName())
		}
		flags := map[string]any{}
		for fk, fv := range gv.AsValueMap() {
			if ft := fv.Type(); ft.IsObjectType() || ft.IsMapType() || ft.IsTupleType() || ft.IsListType() {
				return nil, fmt.Errorf("service %q: arguments.values.%s.%s must be a scalar flag value, got %s", svc, name, fk, ft.FriendlyName())
			}
			flags[fk] = ctyToAny(fv)
		}
		groups[name] = flags
	}
	return groups, nil
}

// pkgCoord is the parsed `package` field: either a string (verbatim in
// raw) or an object { name, backend?, version }.
type pkgCoord struct {
	raw     string
	name    string
	backend string
	version string
	isObj   bool
}

// evalPackage evaluates the polymorphic `package` expression. Returns nil
// when the field is absent/null. A string yields raw; an object yields
// name/backend/version (unknown keys are rejected).
func (r *resolver) evalPackage(expr hcl.Expression, self map[string]cty.Value, dirs srcDirs, field string) (*pkgCoord, error) {
	if expr == nil {
		return nil, nil
	}
	ctx := r.ctxWith(self, dirs)
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: %s", field, diags.Error())
	}
	if val.IsNull() {
		return nil, nil
	}
	t := val.Type()
	switch {
	case t == cty.String:
		return &pkgCoord{raw: strings.TrimSpace(val.AsString())}, nil
	case t.IsObjectType() || t.IsMapType():
		c := &pkgCoord{isObj: true}
		m := val.AsValueMap()
		for k := range m {
			switch k {
			case "name", "backend", "version":
			default:
				return nil, fmt.Errorf("%s: unknown key %q (want name, backend, version)", field, k)
			}
		}
		get := func(k string) (string, error) {
			v, ok := m[k]
			if !ok || v.IsNull() {
				return "", nil
			}
			if v.Type() != cty.String {
				return "", fmt.Errorf("%s.%s must be a string, got %s", field, k, v.Type().FriendlyName())
			}
			return strings.TrimSpace(v.AsString()), nil
		}
		var err error
		if c.name, err = get("name"); err != nil {
			return nil, err
		}
		if c.backend, err = get("backend"); err != nil {
			return nil, err
		}
		if c.version, err = get("version"); err != nil {
			return nil, err
		}
		return c, nil
	default:
		return nil, fmt.Errorf("%s must be a string or { name, backend?, version } object, got %s", field, t.FriendlyName())
	}
}

// miseSpec resolves the coordinate to a mise tool ref (optionally
// backend-qualified, e.g. "aqua:etcd-io/etcd") and a pinned version.
// name and version are both required.
func (c *pkgCoord) miseSpec() (name, version string, err error) {
	if c.isObj {
		if c.name == "" {
			return "", "", fmt.Errorf("package { name } is required")
		}
		if c.version == "" {
			return "", "", fmt.Errorf("package { version } is required")
		}
		name = c.name
		if c.backend != "" {
			name = c.backend + ":" + c.name
		}
		return name, c.version, nil
	}
	at := strings.LastIndex(c.raw, "@")
	if at <= 0 || at == len(c.raw)-1 {
		return "", "", fmt.Errorf("package %q must include a version (e.g. name@version)", c.raw)
	}
	return c.raw[:at], c.raw[at+1:], nil
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
func (r *resolver) evalStrList(expr hcl.Expression, self map[string]cty.Value, field string, dirs srcDirs) ([]string, error) {
	if expr == nil {
		return nil, nil
	}
	ctx := r.ctxWith(self, dirs)
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

// evalCmd evaluates runtime.cmd. Beyond ctxWith it exposes
// tpl::render::flags("<group>"), bound to this service's resolved argument
// groups + options, and flattens one level so a rendered group's tokens
// splice into the argv in place (program <globals> sub <sub-flags>).
func (r *resolver) evalCmd(expr hcl.Expression, self map[string]cty.Value, dirs srcDirs, groups map[string]map[string]any, opts *ArgOptions, toolchain string) ([]string, error) {
	if expr == nil {
		return nil, nil
	}
	ctx := r.ctxWith(self, dirs)
	ctx.Functions["tpl::render::flags"] = renderFlagsFunc(groups, opts, toolchain)
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("runtime.cmd: %s", diags.Error())
	}
	if val.IsNull() {
		return nil, nil
	}
	if t := val.Type(); !t.IsTupleType() && !t.IsListType() {
		return nil, fmt.Errorf("runtime.cmd must be a list, got %s", t.FriendlyName())
	}
	var out []string
	for _, ev := range val.AsValueSlice() {
		et := ev.Type()
		switch {
		case et == cty.String:
			out = append(out, ev.AsString())
		case et == cty.Number || et == cty.Bool:
			out = append(out, fmt.Sprintf("%v", ctyToAny(ev)))
		case et.IsTupleType() || et.IsListType():
			for _, sub := range ev.AsValueSlice() {
				if sub.Type() != cty.String {
					return nil, fmt.Errorf("runtime.cmd: a spliced list (tpl::render::flags) must contain strings, got %s", sub.Type().FriendlyName())
				}
				out = append(out, sub.AsString())
			}
		default:
			return nil, fmt.Errorf("runtime.cmd elements must be strings or string lists, got %s", et.FriendlyName())
		}
	}
	return out, nil
}

// renderFlagsFunc backs tpl::render::flags("<group>"): it renders one named
// argument group of the current service into argv tokens, honoring the
// service's options (and Ruby's forced space). Unknown group ⇒ error.
func renderFlagsFunc(groups map[string]map[string]any, opts *ArgOptions, toolchain string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "group", Type: cty.String}},
		Type:   function.StaticReturnType(cty.List(cty.String)),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			name := args[0].AsString()
			grp, ok := groups[name]
			if !ok {
				defined := make([]string, 0, len(groups))
				for k := range groups {
					defined = append(defined, k)
				}
				sort.Strings(defined)
				return cty.NilVal, fmt.Errorf("tpl::render::flags(%q): no such argument group; defined: %s", name, strings.Join(defined, ", "))
			}
			toks := renderFlags(grp, opts, toolchain)
			if len(toks) == 0 {
				return cty.ListValEmpty(cty.String), nil
			}
			vals := make([]cty.Value, len(toks))
			for i, t := range toks {
				vals[i] = cty.StringVal(t)
			}
			return cty.ListVal(vals), nil
		},
	})
}

// evalStrOrList evaluates an expression that may be either a single string
// or a list of strings, always yielding []string. A bare string becomes a
// one-element slice. Nil/null ⇒ nil. Used by `dotenv`, which accepts both
// `dotenv = ".env"` and `dotenv = [".env", ".env.local"]`.
func (r *resolver) evalStrOrList(expr hcl.Expression, self map[string]cty.Value, field string, dirs srcDirs) ([]string, error) {
	if expr == nil {
		return nil, nil
	}
	ctx := r.ctxWith(self, dirs)
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: %s", field, diags.Error())
	}
	if val.IsNull() {
		return nil, nil
	}
	if val.Type() == cty.String {
		return []string{val.AsString()}, nil
	}
	return r.evalStrList(expr, self, field, dirs)
}

// evalStr evaluates an expression expected to be a string (the `sudo`
// snippet). Nil expression ⇒ "".
func (r *resolver) evalStr(expr hcl.Expression, self map[string]cty.Value, field string, dirs srcDirs) (string, error) {
	if expr == nil {
		return "", nil
	}
	ctx := r.ctxWith(self, dirs)
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
func (r *resolver) ctxWith(self map[string]cty.Value, dirs srcDirs) *hcl.EvalContext {
	// `never` is the keyword that marks a provision latent
	// (`after = never`). Defined as a plain string so the bare
	// identifier resolves; it never matches a real barrier ref ('@').
	vars := map[string]cty.Value{"never": cty.StringVal(neverSentinel)}
	if len(r.serviceByTC) > 0 {
		toolchains := map[string]cty.Value{}
		for tc, services := range r.serviceByTC {
			toolchains[tc] = cty.ObjectVal(copyCtyMap(services))
		}
		vars["service"] = cty.ObjectVal(toolchains)
	}
	if len(r.toolchainCty) > 0 {
		vars["toolchain"] = cty.ObjectVal(copyCtyMap(r.toolchainCty))
	}
	// fs::src / src::hash exist only in a SERVICE scope — they read `checkout`,
	// which the caller passes as the current service's checkout, or "" at file
	// scope (top-level dotenv/sysenv, toolchain), where they then error clearly
	// instead of leaking a prior service's stale checkout.
	if self != nil {
		vars["self"] = cty.ObjectVal(copyCtyMap(self))
	}
	return &hcl.EvalContext{
		Variables: vars,
		Functions: r.functions(dirs),
	}
}

func copyCtyMap(in map[string]cty.Value) map[string]cty.Value {
	out := make(map[string]cty.Value, len(in))
	maps.Copy(out, in)
	return out
}

// ArgSentinel is the placeholder a `${self.runtime.provision.<p>.arguments.<a>}`
// reference resolves to at configure time. NUL-delimited so it can't collide
// with real shell/HCL text; alpha replaces it with the supplied value at
// invoke. JSON escapes the NUL bytes, so the sentinel survives the control wire.
func ArgSentinel(name string) string { return "\x00zarg:" + name + "\x00" }

// BinSentinel is the placeholder fs::service::bin / fs::toolchain::bin resolve
// to at configure time: a NUL-framed token "<kind>:<ref>" where kind is "svc"
// (ref = a service id, e.g. "service.pkg.postgres") or "tc" (ref = a toolchain
// key, e.g. "go"). alpha swaps it for the resolved bin dir at provision-run
// time — the dir is only known once mise has installed the toolchain, so it
// can't be a concrete eval value. Same NUL framing as ArgSentinel; neither
// kind nor ref carries ':' so "<kind>:<ref>" splits unambiguously.
func BinSentinel(kind, ref string) string { return "\x00zbin:" + kind + ":" + ref + "\x00" }

const envOpPrefix = "\x00zenvop:"

// EnvOpSentinel is the placeholder env::prepend / env::append resolve to: a
// NUL-framed directive "op\x00dir1\x00dir2…" describing a PATH-style list
// mutation alpha applies to the already-assembled value at provision-run
// time (prepend/append the dirs to the var named by the env map key). args
// may be BinSentinels — those are swapped to real dirs by SubstituteBins
// BEFORE ParseEnvOp runs, so the directive only ever gets split once its dirs
// are NUL-free.
func EnvOpSentinel(op string, args []string) string {
	return envOpPrefix + op + "\x00" + strings.Join(args, "\x00") + "\x00"
}

// ParseEnvOp recovers an env-op directive produced by EnvOpSentinel. ok is
// false for any value that isn't one (a plain env value), so callers treat
// non-directive entries as ordinary overlays. Must run AFTER SubstituteBins —
// split assumes the dirs carry no NUL.
func ParseEnvOp(value string) (op string, args []string, ok bool) {
	if !strings.HasPrefix(value, envOpPrefix) || !strings.HasSuffix(value, "\x00") {
		return "", nil, false
	}
	parts := strings.Split(value[len(envOpPrefix):len(value)-1], "\x00")
	return parts[0], parts[1:], true
}

// SubstituteBins replaces every BinSentinel in s with the dirs resolve returns
// for (kind, ref), joined by the path-list separator. Mirrors substituteArgs:
// a plain string pass over snippets and env values before they reach a shell
// or ParseEnvOp. resolve returning no dirs drops the token (the binary then
// falls back to whatever PATH already has).
func SubstituteBins(s string, resolve func(kind, ref string) []string) string {
	const pre = "\x00zbin:"
	for {
		i := strings.Index(s, pre)
		if i < 0 {
			return s
		}
		rest := s[i+len(pre):]
		j := strings.IndexByte(rest, 0)
		if j < 0 {
			return s // malformed (unterminated token); leave the rest as-is
		}
		token := s[i : i+len(pre)+j+1]
		var dirs string
		if kind, ref, ok := strings.Cut(rest[:j], ":"); ok {
			dirs = strings.Join(resolve(kind, ref), string(zfs.PathListSeparator))
		}
		s = strings.Replace(s, token, dirs, 1)
	}
}

// provisionSelfWithArgs returns a copy of self where the named provision's node
// (self.runtime.provision.<provName>) gains an `arguments` object mapping each
// declared arg name to its placeholder sentinel. Only that one provision's node
// is augmented, so any reference to another provision's `.arguments` resolves
// against a node without the attribute and errors. No-op when the provision
// declares no arguments.
func provisionSelfWithArgs(self map[string]cty.Value, provName string, argNames []string) map[string]cty.Value {
	if len(argNames) == 0 {
		return self
	}
	argsObj := make(map[string]cty.Value, len(argNames))
	for _, n := range argNames {
		argsObj[n] = cty.StringVal(ArgSentinel(n))
	}
	out := copyCtyMap(self)
	rt := self["runtime"].AsValueMap()
	provs := rt["provision"].AsValueMap()
	node := provs[provName].AsValueMap()
	node["arguments"] = cty.ObjectVal(argsObj)
	provs[provName] = cty.ObjectVal(node)
	rt["provision"] = cty.ObjectVal(provs)
	out["runtime"] = cty.ObjectVal(rt)
	return out
}

// evalProvisionArgs parses a provision's `argument` blocks into resolved
// ProvisionArgs: it defaults and validates the type and evaluates the optional
// default expression to a concrete value (against the service `self`).
func (r *resolver) evalProvisionArgs(pb *provisionBlock, self map[string]cty.Value, dirs srcDirs, label string) ([]*ProvisionArg, error) {
	if len(pb.Argument) == 0 {
		return nil, nil
	}
	out := make([]*ProvisionArg, 0, len(pb.Argument))
	for _, ab := range pb.Argument {
		typ := ab.Type
		if typ == "" {
			typ = "string"
		}
		if typ != "string" && typ != "number" && typ != "bool" {
			return nil, fmt.Errorf("%s.argument %q: type must be string|number|bool, got %q", label, ab.Name, typ)
		}
		var def any
		if ab.Default != nil {
			dv, diags := ab.Default.Value(r.ctxWith(self, dirs))
			if diags.HasErrors() {
				return nil, fmt.Errorf("%s.argument %q.default: %s", label, ab.Name, diags.Error())
			}
			def = ctyScalar(dv)
		}
		out = append(out, &ProvisionArg{Name: ab.Name, Type: typ, Required: ab.Required, Default: def, Description: ab.Description})
	}
	return out, nil
}

// ctyScalar projects a scalar cty.Value (string/number/bool) into a Go value
// for ProvisionArg.Default. Non-scalars and null yield nil.
func ctyScalar(v cty.Value) any {
	if v.IsNull() {
		return nil
	}
	switch v.Type() {
	case cty.String:
		return v.AsString()
	case cty.Bool:
		return v.True()
	case cty.Number:
		bf := v.AsBigFloat()
		if i, acc := bf.Int64(); acc == big.Exact {
			return i
		}
		f, _ := bf.Float64()
		return f
	default:
		return nil
	}
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

// argsSelfCty exposes resolved argument groups under self.arguments.values,
// so self.arguments.values.<group>.<flag> resolves to a concrete value.
func argsSelfCty(groups map[string]map[string]any) cty.Value {
	valuesObj := make(map[string]cty.Value, len(groups))
	for name, flags := range groups {
		valuesObj[name] = mapValToCty(flags)
	}
	return cty.ObjectVal(map[string]cty.Value{"values": cty.ObjectVal(valuesObj)})
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

func (r *resolver) functions(dirs srcDirs) map[string]function.Function {
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
		"fs::tmp":   str(func() string { return r.inv.TmpDir }),   // generated files
		"fs::src":   str(func() string { return dirs.root }),      // src.path: checkout root (service scope only)
		"fs::exe":   str(func() string { return dirs.exe }),       // src.path + src.exe: exe-anchored work dir (= self.dir)
		"fs::bin":   str(func() string { return r.inv.BinDir() }), // build outputs (outside src)
		"fs::state": str(func() string { return r.inv.StateDir }), // per-workspace state root (workspaces/<wt>)
		"fs::etc":   str(func() string { return dirs.etc }),       // <StateDir>/etc/<svc>: generated config that must persist (service scope only)
		"fs::var":   str(func() string { return dirs.vardir }),    // <StateDir>/var/<svc>: variable runtime state — db/logs (service scope only)
		"fs::hash":  r.fsHashFunc(),                               // instance identity (location)
		// cfg:: namespace — manifest identity (Alphasfile bytes + parent ctx).
		"cfg::hash": r.cfgHashFunc(),
		// src:: namespace — current service's source code identity.
		"src::hash": r.srcHashFunc(dirs.root),

		"net::pickport": pickPortFunc(),
		"os::env":       osEnvFunc(),

		// Cross-toolchain binary access, all resolved at provision-run time
		// via deferred sentinels (dirs aren't known until mise installs):
		//   fs::service::bin(service.<tc>.<name>) — a service's package bins
		//     (the handle for pkg, which has no standalone toolchain ref).
		//   fs::toolchain::bin(toolchain.<lang>) — a toolchain's bin/tool
		//     world (e.g. a Go `go install` tool used from a Ruby project).
		//   env::prepend/append put dirs onto a PATH-style var.
		"fs::service::bin":   svcBinFunc(),
		"fs::service::etc":   r.svcPathFunc("etc"),
		"fs::service::var":   r.svcPathFunc("var"),
		"fs::toolchain::bin": tcBinFunc(),
		"env::prepend":       envOpFunc("prepend"),
		"env::append":        envOpFunc("append"),

		// test:: namespace — observation/control primitives for the
		// conformance harness. Available only when zordon runs with
		// $ZORDON_TEST_HARNESS=1 (set by the harness on every spawn);
		// in production they error out so users can't accidentally
		// build them into a real Alphasfile.
		"test::log":  testLogFunc(r.testCfg),
		"test::fail": testFailFunc(r.testCfg),
	}
}

// evalStaticSrcPath evaluates a src{path} expression without a live
// invocation — the parse-only context ParseServices (and `zordon
// workspace`) runs in. Only host-level helpers (os::env) are available;
// invocation/identity namespaces (fs::, cfg::, src::, net::, self.*)
// are absent, so referencing them is a clear eval error rather than a
// silent empty string. Returns "" when src{path} is absent.
func evalStaticSrcPath(src *srcBlock) (string, error) {
	if src.Path == nil {
		return "", nil
	}
	ctx := &hcl.EvalContext{Functions: map[string]function.Function{
		"os::env": osEnvFunc(),
	}}
	val, diags := src.Path.Value(ctx)
	if diags.HasErrors() {
		return "", fmt.Errorf("src.path: %s", diags.Error())
	}
	if val.IsNull() {
		return "", nil
	}
	if val.Type() != cty.String {
		return "", fmt.Errorf("src.path must be a string, got %s", val.Type().FriendlyName())
	}
	return val.AsString(), nil
}

// svcBinFunc implements fs::service::bin(<ref>). The arg is a service data
// leaf — `self` or `service.<tc>.<name>` — an object carrying name and
// toolchain. We validate that shape (mirroring provisionRefOf) and emit a
// "svc" BinSentinel naming the service id; alpha resolves it to the bin dir
// at provision-run time. A non-existent service ref fails earlier in HCL's
// own traversal ("Unsupported attribute"), so it never reaches here.
func svcBinFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "service", Type: cty.DynamicPseudoType}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			v := args[0]
			t := v.Type()
			if v.IsNull() || !t.IsObjectType() || !t.HasAttribute("name") || !t.HasAttribute("toolchain") {
				return cty.NilVal, fmt.Errorf("fs::service::bin: expected a service reference (self or service.<tc>.<name>), got %s", t.FriendlyName())
			}
			tc, name := v.GetAttr("toolchain"), v.GetAttr("name")
			if tc.Type() != cty.String || name.Type() != cty.String {
				return cty.NilVal, errors.New("fs::service::bin: service reference has a non-string name/toolchain")
			}
			return cty.StringVal(BinSentinel("svc", serviceID(tc.AsString(), name.AsString()))), nil
		},
	})
}

// svcPathFunc implements fs::service::etc / fs::service::var — the per-service
// persistent dir (<StateDir>/<sub>/<svc>) of a same-invocation service named by
// a `self` or `service.<tc>.<name>` reference. Unlike fs::service::bin these are
// pure StateDir joins known at eval, so they resolve to the concrete path here
// rather than a deferred sentinel. Scope is the current invocation (all
// same-alpha services share one StateDir); a federation parent's dir is reached
// through its exported vars, not here. sub is "etc" or "var".
func (r *resolver) svcPathFunc(sub string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "service", Type: cty.DynamicPseudoType}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if r.inv == nil || r.inv.StateDir == "" {
				return cty.NilVal, fmt.Errorf("fs::service::%s() called but no invocation configured", sub)
			}
			v := args[0]
			t := v.Type()
			if v.IsNull() || !t.IsObjectType() || !t.HasAttribute("name") || !t.HasAttribute("toolchain") {
				return cty.NilVal, fmt.Errorf("fs::service::%s: expected a service reference (self or service.<tc>.<name>), got %s", sub, t.FriendlyName())
			}
			name := v.GetAttr("name")
			if name.Type() != cty.String {
				return cty.NilVal, fmt.Errorf("fs::service::%s: service reference has a non-string name", sub)
			}
			return cty.StringVal(filepath.Join(r.inv.StateDir, sub, name.AsString())), nil
		},
	})
}

// tcBinFunc implements fs::toolchain::bin(toolchain.<lang>). The arg is a
// toolchain ref — an object whose barrier attrs (e.g. "ready") read
// "toolchain.<key>@<state>". We recover <key> and emit a "tc" BinSentinel;
// alpha resolves it to that toolchain's bin dir(s) at provision-run time.
// This is the handle for a tool installed into a toolchain's world (e.g. a
// Go `go install` tool) that no service references.
func tcBinFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "toolchain", Type: cty.DynamicPseudoType}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			v := args[0]
			t := v.Type()
			if v.IsNull() || !t.IsObjectType() || !t.HasAttribute("ready") {
				return cty.NilVal, fmt.Errorf("fs::toolchain::bin: expected a toolchain reference (toolchain.<lang>), got %s", t.FriendlyName())
			}
			ready := v.GetAttr("ready")
			if ready.Type() != cty.String {
				return cty.NilVal, errors.New("fs::toolchain::bin: malformed toolchain reference")
			}
			id, _, _ := strings.Cut(ready.AsString(), "@") // "toolchain.<key>"
			key, ok := strings.CutPrefix(id, "toolchain.")
			if !ok || key == "" {
				return cty.NilVal, fmt.Errorf("fs::toolchain::bin: not a toolchain reference: %q", ready.AsString())
			}
			return cty.StringVal(BinSentinel("tc", key)), nil
		},
	})
}

// osEnvFunc reads a host environment variable at evaluation time (in the
// zordon process, so it sees your shell env). os::env("NAME") errors if
// NAME is unset; os::env("NAME", "default") returns the default instead.

// envOpFunc implements env::prepend / env::append. Variadic string args are
// the dirs (literals or fs::service::bin sentinels) to layer onto the
// PATH-style var named by the env map key — applied at provision-run time
// against the already-assembled value (see EnvOpSentinel / ParseEnvOp).
func envOpFunc(op string) function.Function {
	return function.New(&function.Spec{
		Params:   []function.Parameter{{Name: "dir", Type: cty.String}},
		VarParam: &function.Parameter{Name: "dirs", Type: cty.String},
		Type:     function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			dirs := make([]string, 0, len(args))
			for _, a := range args {
				if a.IsNull() {
					return cty.NilVal, fmt.Errorf("env::%s: arguments must be non-null strings", op)
				}
				dirs = append(dirs, a.AsString())
			}
			return cty.StringVal(EnvOpSentinel(op, dirs)), nil
		},
	})
}

func osEnvFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "name", Type: cty.String}},
		VarParam: &function.Parameter{Name: "default", Type: cty.String,
			AllowNull: true},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			name := args[0].AsString()
			if v, ok := zenv.Lookup(name); ok {
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
// $HOME; a relative path resolves against the Alphasfile's OWN
// directory (so the same Alphasfile means the same thing regardless of
// where the user ran zordon from — `cd into subdir; zordon start` walks
// up to the same file and gets the same resolved paths). Empty stays
// empty (no dir primary). Workspace invocations adopt the project-root
// Alphasfile, so r.afDir is project root there too.
func (r *resolver) resolveDir(dir string) string {
	if dir == "" {
		return ""
	}
	return resolveSrcDir(r.afDir, dir)
}

// fsHashFunc returns the short (16 hex chars) hash that identifies this
// alpha instance by its filesystem location (invocation dir + workspace).
// Stable per directory across runs and edits; unique across workspaces.
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
			if r.cfgHash == "" {
				return cty.NilVal, errors.New("cfg::hash() called but no manifest identity configured")
			}
			return cty.StringVal(r.cfgHash), nil
		},
	})
}

// srcHashFunc returns the short identity of the current service's source
// code. For a git-tracked checkout (dir/src primary, or a materialized git
// workspace) that's `git rev-parse --short HEAD`; otherwise it errors. Use
// it as a build cache key or a "code generation" stamp — pair with
// fs::hash() when you also need the location.
func (r *resolver) srcHashFunc(root string) function.Function {
	return function.New(&function.Spec{
		Type: function.StaticReturnType(cty.String),
		Impl: func(_ []cty.Value, _ cty.Type) (cty.Value, error) {
			dir := root
			if dir == "" {
				return cty.NilVal, errors.New("src::hash(): no source primary for this service (use-only or no checkout)")
			}
			if _, err := zfs.Stat(filepath.Join(dir, ".git")); err != nil {
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
			port, err := pickFreePort()
			if err != nil {
				return cty.NilVal, fmt.Errorf("net::pickport: %w", err)
			}
			return cty.NumberIntVal(int64(port)), nil
		},
	})
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close()
	return addr.Port, nil
}

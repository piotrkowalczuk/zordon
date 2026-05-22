package alphasfile

import (
	"errors"
	"fmt"
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
func (r *resolver) provisionRefOf(expr hcl.Expression, self map[string]cty.Value) (string, bool, error) {
	if expr == nil {
		return "", false, nil
	}
	val, diags := expr.Value(r.ctxWith(self))
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
	// afDir is the absolute directory of the Alphasfile we're resolving.
	// Relative `src` and `dir` paths interpret against this — NOT the
	// invocation's working directory — so an Alphasfile means the same
	// thing whether the user runs zordon from project root or from a
	// subdir (walkUp finds the same Alphasfile either way).
	afDir string
	root  *rootBlock
	inv   *invocation.Invocation

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

	// checkout dir of the service currently being evaluated (fs::src()).
	curCheckout string

	// testCfg carries the conformance-harness gating + log path the
	// test:: HCL functions need; zero value disables them.
	testCfg TestConfig

	resolvedServices []*Service
}

func resolve(name string, root *rootBlock, inv *invocation.Invocation, parent *ParentContext, testCfg TestConfig) (*Alphasfile, error) {
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
		inv:         inv,
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
		for k, v := range parent.toolchain {
			toolchain[k] = v
		}
	}
	if root.Toolchain != nil {
		for lang, sub := range root.Toolchain.byLabel() {
			envMap, err := r.evalMap(sub.Env, nil, "toolchain."+lang+".env")
			if err != nil {
				return nil, err
			}
			toolsMap, err := r.evalMap(sub.Tools, nil, "toolchain."+lang+".tools")
			if err != nil {
				return nil, err
			}
			toolchain[lang] = &ToolchainConfig{
				Version: sub.Version,
				Tools:   toStringMap(toolsMap),
				Env:     toStringMap(envMap),
			}
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
	for _, n := range order {
		st := states[n.svcID]
		if err := r.evalProducerNode(n, st); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", n.svcID, producerLabel(n), err)
		}
	}

	// Sinks per service: runtime.cmd, build.cmd, build/runtime/agent env,
	// dotenv, print, sudo, provisions, readiness, wire-stable Service
	// finalization. Cross-service references in sinks reach into other
	// services' fully-evaluated producers via r.serviceByTC; intra-
	// service sinks read the populated st.self. Sink-to-sink references
	// across services aren't supported (sinks aren't published back to
	// self/serviceByTC), so sink order doesn't matter.
	for _, sb := range root.Services {
		sid := serviceID(sb.Toolchain, sb.Name)
		if err := r.finishService(states[sid]); err != nil {
			return nil, fmt.Errorf("%s: %w", sid, err)
		}
	}
	// File-level dotenv: top-level, no `self`; may use fs::/cfg:: funcs.
	gdot, err := r.evalStrOrList(root.Dotenv, nil, "dotenv")
	if err != nil {
		return nil, err
	}
	if len(toolchain) == 0 {
		toolchain = nil
	}
	// SysEnv: parent's accumulated whitelist + this level's own.
	// Order-preserving union (see mergeSysEnv) so cfg::hash() is stable.
	localSysEnv, err := r.evalStrList(root.SysEnv, nil, "sysenv")
	if err != nil {
		return nil, err
	}
	var parentSysEnv []string
	if parent != nil {
		parentSysEnv = parent.sysenv
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
	return &Alphasfile{
		Dotenv:    gdot,
		Services:  r.resolvedServices,
		Toolchain: toolchain,
		SysEnv:    sysEnv,
	}, nil
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
	sb          *serviceBlock
	id          string // "service.<tc>.<name>"
	dir         string
	self        map[string]cty.Value // grows: name/toolchain/dir/runtime/build → +vars/+env/+args/+file
	files       []*File
	fileVals    map[string]cty.Value
	vars        map[string]any
	args        map[string]any
	envMap      map[string]any
	curCheckout string // value fs::src() returns while this service's expressions evaluate
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
		//   src-only in the "main" worktree → the src dir itself, used
		//     IN PLACE (your live working tree; zordon never copies or
		//     git-worktrees it, so uncommitted edits just work — the
		//     edit→start loop). "main" is only a mental label; in
		//     practice it means "use src directly".
		//   anything else (named worktree, or a git primary) → a per-
		//     invocation checkout alpha materializes via git worktree
		//     add.
		//
		// Pure: no filesystem touch here.
		dir := ""
		var gitURL, srcPath string
		if sb.Git != nil {
			gitURL = sb.Git.URL
		}
		if sb.Src != nil {
			srcPath = sb.Src.Path
		}
		switch {
		case srcPath != "" && gitURL == "" && r.inv.Worktree == invocation.MainWorktree:
			dir = r.resolveDir(srcPath)
		case gitURL != "" || srcPath != "":
			dir = r.inv.CheckoutPath(sb.Name)
		}

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

		st := &svcState{
			sb:          sb,
			id:          sid,
			dir:         dir,
			self:        self,
			fileVals:    map[string]cty.Value{},
			curCheckout: dir,
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
	r.curCheckout = st.curCheckout
	switch n.kind {
	case kindVars:
		if st.sb.Vars == nil {
			return nil
		}
		v, err := r.evalMap(st.sb.Vars, st.self, "vars")
		if err != nil {
			return err
		}
		st.vars = v
		st.self["vars"] = mapValToCty(v)
	case kindArguments:
		if st.sb.Arguments == nil {
			return nil
		}
		a, err := r.evalMap(st.sb.Arguments, st.self, "arguments")
		if err != nil {
			return err
		}
		st.args = a
		st.self["arguments"] = mapValToCty(a)
	case kindEnv:
		if st.sb.Env == nil {
			return nil
		}
		e, err := r.evalMap(st.sb.Env, st.self, "env")
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
		path, body, err := evalFileExpr(fb, r.ctxWith(st.self))
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
	r.curCheckout = st.curCheckout

	// Sinks: consume the fully-populated self; order among them is
	// irrelevant since nothing reads them back.
	var (
		command []string
		err     error
	)
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
	var runtimeAfter []string
	if sb.Runtime != nil {
		if runEnv, err = phaseEnv(sb.Runtime.Env, "runtime.env"); err != nil {
			return err
		}
		runtimeAfter, err = r.evalStrList(sb.Runtime.After, self, "runtime.after")
		if err != nil {
			return err
		}
	}
	if sb.Agent != nil {
		if agentEnv, err = phaseEnv(sb.Agent.Env, "agent.env"); err != nil {
			return err
		}
	}
	dotenv, err := r.evalStrOrList(sb.Dotenv, self, "dotenv")
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
			// cmd is either an inline shell snippet (string) or a bare
			// reference to another (latent) provision used as a template.
			cmdRef, isRef, err := r.provisionRefOf(pb.Cmd, self)
			if err != nil {
				return fmt.Errorf("%s.cmd: %w", label, err)
			}
			var cmd string
			if isRef {
				if cmdRef == st.id+".runtime.provision."+pb.Name {
					return fmt.Errorf("%s.cmd: provision cannot reference itself", label)
				}
			} else {
				cmd, err = r.evalStr(pb.Cmd, self, label+".cmd")
				if err != nil {
					return err
				}
			}
			verify, err := r.evalStr(pb.Verify, self, label+".verify")
			if err != nil {
				return err
			}
			if isRef && (strings.TrimSpace(check) != "" || strings.TrimSpace(verify) != "") {
				return fmt.Errorf("%s: check/verify are not allowed when cmd references another provision (the template owns them)", label)
			}
			var penv map[string]string
			if pb.Env != nil {
				m, err := r.evalMap(pb.Env, self, label+".env")
				if err != nil {
					return err
				}
				penv = toStringMap(m)
			}
			// `after` accepts the bare keyword `never` (a latent
			// provision: registered but not auto-run) OR a list of
			// barrier refs — never both.
			afterList, err := r.evalStrOrList(pb.After, self, label+".after")
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
			provisions = append(provisions, &ProvisionStep{
				Name:     pb.Name,
				Check:    check,
				Cmd:      cmd,
				Verify:   verify,
				Env:      penv,
				After:    afterClean,
				Detached: pb.Detached,
				Latent:   latent,
				CmdRef:   cmdRef,
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
		Vars:           st.vars,
		Arguments:      st.args,
		Env:            toStringMap(st.envMap),
		BuildEnv:       buildEnv,
		RunEnv:         runEnv,
		AgentEnv:       agentEnv,
		Dotenv:         dotenv,
		Command:        command,
		After:          runtimeAfter,
		Sudo:           sudo,
		Provision:      provisions,
		Files:          st.files,
		Dir:            st.dir,
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
	case ToolchainGo, ToolchainRust, ToolchainRuby, ToolchainNode:
	default:
		return fmt.Errorf("unknown toolchain %q (want go|rust|ruby|nodejs)", sb.Toolchain)
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
		install = strings.TrimSpace(sb.Package)
	case ToolchainRust:
		if sb.Package != "" {
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
		if sb.Package != "" || sb.Crate != nil {
			return fmt.Errorf("service %q: %s has no use-only mode", sb.Name, sb.Toolchain)
		}
		if len(sb.Features) > 0 {
			return fmt.Errorf("service %q: features is rust-only", sb.Name)
		}
	}

	// Modes: use-only (install), remote-worktree (git block), or
	// local-worktree (src{path}). git+src{exe} (no path) is allowed:
	// src carries only the build subdir inside the cloned remote.
	var srcGitURL, srcLocalPath, srcBranch, srcTag, srcRev, srcExe string
	if sb.Git != nil {
		srcGitURL = strings.TrimSpace(sb.Git.URL)
		srcBranch = sb.Git.Branch
		srcTag = sb.Git.Tag
		srcRev = sb.Git.Rev
	}
	if sb.Src != nil {
		srcLocalPath = sb.Src.Path
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

	var worktree *Worktree
	if sb.Worktree != nil && len(sb.Worktree.Sparse) > 0 {
		// Sparse cone paths are relative to the primary repo root (what
		// `git sparse-checkout set` expects) — pass verbatim, not joined
		// to an absolute Alphasfile path.
		worktree = &Worktree{Sparse: cleanSparse(sb.Worktree.Sparse)}
	}

	var dbg *DebuggerConfig
	var agent *ServiceAgent
	if sb.Debugger != nil && sb.Debugger.Enabled {
		d, a, err := r.evalDebugger(sb, self, command)
		if err != nil {
			return err
		}
		dbg = d
		agent = a
	}

	// For use-only-from-git (rust crate { git, branch/tag/rev }) the git
	// fields ride along in Package.Git/Branch/Tag/Rev — the alpha-side
	// command builder reads them when Install != "". For worktree mode
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
			Worktree:  worktree,
			// In-place: src-only in the "main" worktree → alpha builds/runs
			// from src as-is (no git worktree add, no HEAD reset).
			InPlace: srcLocalPath != "" && r.inv.Worktree == invocation.MainWorktree,
		},
		Debugger: dbg,
		Agent:    agent,
	}
	r.resolvedServices = append(r.resolvedServices, svc)
	return nil
}

func (r *resolver) evalDebugger(sb *serviceBlock, self map[string]cty.Value, command []string) (*DebuggerConfig, *ServiceAgent, error) {
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
		return nil, nil, fmt.Errorf("service %q: debugger.enabled = true wraps runtime itself (`dlv exec <binary> -- <args>`), which is incompatible with an explicit `runtime.cmd`. Use `arguments = {…}` for flags, or set `debugger.wrap_runtime = false` to keep your own cmd", sb.Name)
	}
	port, err := r.evalDebuggerPort(db.Port, self)
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
func (r *resolver) evalDebuggerPort(expr hcl.Expression, self map[string]cty.Value) (int, error) {
	if expr != nil {
		val, diags := expr.Value(r.ctxWith(self))
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

// evalStrOrList evaluates an expression that may be either a single string
// or a list of strings, always yielding []string. A bare string becomes a
// one-element slice. Nil/null ⇒ nil. Used by `dotenv`, which accepts both
// `dotenv = ".env"` and `dotenv = [".env", ".env.local"]`.
func (r *resolver) evalStrOrList(expr hcl.Expression, self map[string]cty.Value, field string) ([]string, error) {
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
	if val.Type() == cty.String {
		return []string{val.AsString()}, nil
	}
	return r.evalStrList(expr, self, field)
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
		"fs::tmp":   str(func() string { return r.inv.TmpDir }),    // generated files
		"fs::src":   str(func() string { return r.curCheckout }),   // this service's checkout
		"fs::bin":   str(func() string { return r.inv.BinDir() }),  // build outputs (outside src)
		"fs::state": str(func() string { return r.inv.StateDir }), // per-worktree state root (.zordon/worktrees/<wt>)
		"fs::hash":  r.fsHashFunc(),                                // instance identity (location)
		// cfg:: namespace — manifest identity (Alphasfile bytes + parent ctx).
		"cfg::hash": r.cfgHashFunc(),
		// src:: namespace — current service's source code identity.
		"src::hash": r.srcHashFunc(),

		"net::pickport": pickPortFunc(),
		"os::env":       osEnvFunc(),

		// test:: namespace — observation/control primitives for the
		// conformance harness. Available only when zordon runs with
		// $ZORDON_TEST_HARNESS=1 (set by the harness on every spawn);
		// in production they error out so users can't accidentally
		// build them into a real Alphasfile.
		"test::log":  testLogFunc(r.testCfg),
		"test::fail": testFailFunc(r.testCfg),
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
// empty (no dir primary). Worktree invocations adopt the project-root
// Alphasfile, so r.afDir is project root there too.
func (r *resolver) resolveDir(dir string) string {
	if dir == "" {
		return ""
	}
	return resolveSrcDir(r.afDir, dir)
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

package alphasfile

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

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

func resolve(root *rootBlock, inv *invocation.Invocation, parent map[string]map[string]cty.Value) (*Alphasfile, error) {
	r := &resolver{
		root:        root,
		inv:         inv,
		serviceByTC: map[string]map[string]cty.Value{},
		taken:       map[string]string{},
	}
	parentKnown := map[string]struct{}{}
	for tc, byName := range parent {
		r.serviceByTC[tc] = map[string]cty.Value{}
		for name, v := range byName {
			r.serviceByTC[tc][name] = v
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
	return &Alphasfile{Services: r.resolvedServices}, nil
}

func (r *resolver) evalService(sb *serviceBlock) error {
	if origin, dup := r.taken[sb.Toolchain+"/"+sb.Name]; dup {
		return fmt.Errorf("service %s.%s collides with a %s service of the same name",
			sb.Toolchain, sb.Name, origin)
	}
	r.taken[sb.Toolchain+"/"+sb.Name] = "local"

	// dir = this service's per-invocation checkout (empty if not
	// worktree-able: crate / prebuilt). Pure — no filesystem touch here;
	// alpha materializes the worktree at this path later.
	dir := ""
	if sb.Git != "" || sb.Src != "" {
		dir = r.inv.CheckoutPath(sb.Name)
	}
	r.curCheckout = dir // what fs::src() returns while this service evals

	// Stage 1: self_base — known from labels + primary, before any eval.
	self := map[string]cty.Value{
		"name":      cty.StringVal(sb.Name),
		"toolchain": cty.StringVal(sb.Toolchain),
		"dir":       cty.StringVal(dir),
	}

	// Stage 2: vars (may reference self.{name,toolchain,dir} and any
	// already-evaluated cross-service value).
	varsVal, err := r.evalMap(sb.Vars, self, "vars")
	if err != nil {
		return err
	}
	self["vars"] = mapValToCty(varsVal)

	// Stage 3: nested file blocks (may reference self.vars and previously-
	// declared sibling files via self.file.<earlier_name>).
	var files []*File
	fileVals := map[string]cty.Value{}
	for _, fb := range sb.Files {
		ctx := r.ctxWith(self)
		path, body, err := evalFileExpr(fb, ctx)
		if err != nil {
			return err
		}
		files = append(files, &File{Name: fb.Name, Path: path, Body: body})
		fileVals[fb.Name] = cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal(fb.Name),
			"path": cty.StringVal(path),
			"body": cty.StringVal(body),
		})
		// Make this file visible to siblings declared after it and to the
		// remaining eval stages.
		self["file"] = cty.ObjectVal(fileVals)
	}
	if _, ok := self["file"]; !ok {
		self["file"] = cty.EmptyObjectVal
	}

	// Stage 4: arguments (may reference self.vars and self.file).
	args, err := r.evalMap(sb.Arguments, self, "arguments")
	if err != nil {
		return err
	}
	self["arguments"] = mapValToCty(args)

	// Stage 5: command (may reference self.vars, self.file, self.arguments).
	command, err := r.evalStrList(sb.Cmd, self, "cmd")
	if err != nil {
		return err
	}
	// Stage 6: sudo hooks (may reference everything resolved so far).
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

	// Stage 7: readiness port (may reference everything in self).
	var probePort int
	if sb.Readiness != nil && sb.Readiness.HTTP != nil {
		ctx := r.ctxWith(self)
		probePort, err = evalIntExpr(sb.Readiness.HTTP.Port, ctx, "readiness.http.port")
		if err != nil {
			return err
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
		Command:        command,
		Sudo:           sudo,
		Files:          files,
		Dir:            dir,
		BinDir:         r.inv.BinDir(),
	}
	if sb.Log != nil {
		rt.Log = &LogConfig{Format: sb.Log.Format, Filter: sb.Log.Filter}
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
	if (sb.Git != "" && sb.Src != "") || (sb.Git != "" && sb.Dir != "") {
		return fmt.Errorf("service %q declares both git and dir; pick one primary", sb.Name)
	}
	src := sb.Src
	if src == "" {
		src = sb.Dir
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
			Exe:       sb.Exe,
			Cmd:       strings.Join(command, " "),
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
		// fs:: namespace — per-invocation filesystem coordinates.
		"fs::tmp": str(func() string { return r.inv.TmpDir }),       // generated files
		"fs::src": str(func() string { return r.curCheckout }),      // this service's checkout
		"fs::bin": str(func() string { return r.inv.BinDir() }),     // build outputs (outside src)
		"pathhash":      r.pathhashFunc(),
		"net::pickport": pickPortFunc(),
		// back-compat aliases
		"tmpdir": str(func() string { return r.inv.TmpDir }),
	}
}

// resolveDir turns a `dir` primary into an absolute path. ~ expands to
// $HOME; a relative path resolves against the Alphasfile's project root
// (so a committed example can say dir = "../.." portably). Empty stays
// empty (no dir primary).
func (r *resolver) resolveDir(dir string) string {
	if dir == "" {
		return ""
	}
	if strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, dir[2:])
		}
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Clean(filepath.Join(r.inv.ProjectRoot(), dir))
}

// pathhash returns the short (8 hex chars) hash that identifies this
// Alphasfile by its absolute path — the same token used in the socket and
// state-dir names. Stable per path, unique across paths. Handy for
// collision-free hostnames in a shared reverse proxy:
// "myapp.${pathhash()}.test".
func (r *resolver) pathhashFunc() function.Function {
	return function.New(&function.Spec{
		Type: function.StaticReturnType(cty.String),
		Impl: func(_ []cty.Value, _ cty.Type) (cty.Value, error) {
			if r.inv == nil || r.inv.Hash == "" {
				return cty.NilVal, errors.New("pathhash() called but no invocation configured")
			}
			return cty.StringVal(r.inv.Hash), nil
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

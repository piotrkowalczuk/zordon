package alphasfile

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// pkgNeed is an import of a package from anywhere but the entrypoint's top
// level, with what it passes.
type pkgNeed struct {
	by    string // "package caddy" or "module gw"
	block *importBlock
	set   *pkgSettings
}

// decodeFeatures reads a package's features: a map of name to description.
func (pb *packageBlock) decodeFeatures() error {
	pb.features = map[string]string{}
	if pb.FeaturesRange == (hcl.Range{}) {
		return nil
	}
	shape := fmt.Sprintf(`features of package %q map each name to what it turns on, such as { tls = "Serves HTTPS with a local CA" }`, pb.Name)
	v, diags := pb.Features.Value(nil)
	if diags.HasErrors() {
		return fmt.Errorf("%s: %s: %s", pb.FeaturesRange, shape, diags.Error())
	}
	if v.IsNull() {
		return nil
	}
	if !v.Type().IsObjectType() && !v.Type().IsMapType() {
		return fmt.Errorf("%s: %s", pb.FeaturesRange, shape)
	}
	vals := v.AsValueMap()
	for _, name := range sortedKeys(vals) {
		if !moduleNameRe.MatchString(name) {
			return fmt.Errorf("%s: feature %q of package %q: use letters, digits, '_' or '-' and start with a letter", pb.FeaturesRange, name, pb.Name)
		}
		d := vals[name]
		if d.IsNull() || !d.Type().Equals(cty.String) || strings.TrimSpace(d.AsString()) == "" {
			return fmt.Errorf("%s: feature %q of package %q needs a description of what it turns on, so whoever imports the package can decide", pb.FeaturesRange, name, pb.Name)
		}
		pb.features[name] = d.AsString()
	}
	return nil
}

// passedSettings evaluates what an import passes to a package. Inputs are
// evaluated statically: literals, os::env, enc::* and, inside a package,
// its own inputs; never another service's values, so a package's
// configuration is known before planning.
func passedSettings(ib *importBlock, own *scopeSettings) (*pkgSettings, error) {
	set := &pkgSettings{at: ib.DefRange, inputs: map[string]cty.Value{}, features: slices.Clone(ib.Features)}
	sort.Strings(set.features)
	for i := 1; i < len(set.features); i++ {
		if set.features[i] == set.features[i-1] {
			return nil, fmt.Errorf("%s: import %q lists feature %q twice", ib.DefRange, ib.Path, set.features[i])
		}
	}
	if ib.InputsRange == (hcl.Range{}) {
		return set, nil
	}
	ctx := staticEvalCtx()
	if own != nil {
		ctx.Variables = map[string]cty.Value{"inputs": cty.ObjectVal(own.inputs)}
	}
	v, diags := ib.Inputs.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: import %q inputs: %s", ib.InputsRange, ib.Path, diags.Error())
	}
	if v.IsNull() {
		return set, nil
	}
	if !v.Type().IsObjectType() && !v.Type().IsMapType() {
		return nil, fmt.Errorf("%s: import %q: inputs must be an object such as { name = value }, got %s", ib.InputsRange, ib.Path, v.Type().FriendlyName())
	}
	maps.Copy(set.inputs, v.AsValueMap())
	return set, nil
}

func sameSettings(a, b *pkgSettings) bool {
	if !slices.Equal(a.features, b.features) || len(a.inputs) != len(b.inputs) {
		return false
	}
	for k, av := range a.inputs {
		bv, ok := b.inputs[k]
		if !ok || !av.RawEquals(bv) {
			return false
		}
	}
	return true
}

// requiredSentinel is what `required` evaluates to in a package's inputs:
// a string no real default can collide with, as for `never`.
const requiredSentinel = "\x00zordon:required\x00"

// resolvePackages fixes the inputs and features of every package. A package
// the entrypoint imports at its top level gets what that import passes, and
// every other import of it in the stack must be satisfied by that. Any
// other package gets the union of what its importers in the stack pass, so
// importers are resolved first.
func (t *Tree) resolvePackages() error {
	plain := t.plainActive()
	pending := maps.Clone(t.packages)
	for len(pending) > 0 {
		progressed := false
		for _, name := range sortedKeys(pending) {
			p := pending[name]
			if !p.entry && !t.importersDone(p, pending) {
				continue
			}
			if err := t.resolvePackage(p, plain); err != nil {
				return err
			}
			delete(pending, name)
			progressed = true
		}
		if !progressed {
			return fmt.Errorf("packages %s import each other, so none of them can be configured first; import one of them at the top of the entrypoint to configure it there", strings.Join(sortedKeys(pending), ", "))
		}
	}
	for _, name := range sortedKeys(t.packages) {
		p := t.packages[name]
		if !p.entry {
			continue
		}
		needs, err := t.needsOf(p, plain)
		if err != nil {
			return err
		}
		p.needs = needs
		for _, n := range needs {
			if err := checkSatisfied(p, n); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Tree) resolvePackage(p *pkgInstance, plain map[string]bool) error {
	pb, who := p.file.block, "package "+p.name
	if p.entry {
		var err error
		p.active = true
		p.settings, err = resolveSettings(pb, p.set, who)
		return err
	}
	needs, err := t.needsOf(p, plain)
	if err != nil {
		return err
	}
	if len(needs) == 0 {
		if _, err := declaredInputs(pb, who); err != nil {
			return err
		}
		p.settings = idleSettings(pb)
		return nil
	}
	p.active, p.needs = true, needs
	set, err := mergeNeeds(p, needs)
	if err != nil {
		return err
	}
	p.settings, err = resolveSettings(pb, set, who)
	return err
}

// needsOf lists the imports of p that are part of the stack, other than the
// entrypoint's top level: from scopes outside packages the entrypoint
// reaches, and from packages in the stack whose enabled keeps the import.
func (t *Tree) needsOf(p *pkgInstance, plain map[string]bool) ([]pkgNeed, error) {
	who := "package " + p.name
	declared, err := declaredInputs(p.file.block, who)
	if err != nil {
		return nil, err
	}
	var needs []pkgNeed
	for _, scope := range sortedKeys(t.links) {
		if scope == DefaultModule {
			continue
		}
		owner, inPkg := packageOf(scope)
		for _, l := range t.links[scope] {
			if l.pkg != p || l.block == nil {
				continue
			}
			var own *scopeSettings
			by := "module " + scope
			if inPkg {
				q := t.packages[owner]
				if owner == p.name || !q.active {
					continue
				}
				if l.block.EnabledRange != (hcl.Range{}) {
					on, err := evalEnabled(l.block.Enabled, q.settings)
					if err != nil {
						return nil, err
					}
					if !on {
						continue
					}
				}
				own, by = q.settings, "package "+owner
			} else if !plain[scope] {
				continue
			}
			set, err := passedSettings(l.block, own)
			if err != nil {
				return nil, err
			}
			if err := checkDeclared(p.file.block, declared, set, who); err != nil {
				return nil, err
			}
			needs = append(needs, pkgNeed{by: by, block: l.block, set: set})
		}
	}
	return needs, nil
}

// importersDone reports whether every package that imports p is resolved.
func (t *Tree) importersDone(p *pkgInstance, pending map[string]*pkgInstance) bool {
	for scope, links := range t.links {
		owner, ok := packageOf(scope)
		if !ok || owner == p.name || pending[owner] == nil {
			continue
		}
		for _, l := range links {
			if l.pkg == p {
				return false
			}
		}
	}
	return true
}

// plainActive is the set of scopes outside packages in the stack: the
// entrypoint's top level and modules, and the fragment modules they import,
// transitively. None of them can switch an import off.
func (t *Tree) plainActive() map[string]bool {
	seen := map[string]bool{}
	queue := append([]string{DefaultModule}, sortedKeys(t.files[0].declared)...)
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		if seen[s] {
			continue
		}
		seen[s] = true
		for _, l := range t.links[s] {
			queue = append(queue, l.modules...)
		}
	}
	return seen
}

func mergeNeeds(p *pkgInstance, needs []pkgNeed) (*pkgSettings, error) {
	set := &pkgSettings{at: needs[0].block.DefRange, inputs: map[string]cty.Value{}}
	from := map[string]pkgNeed{}
	on := map[string]bool{}
	for _, n := range needs {
		for _, f := range n.set.features {
			on[f] = true
		}
		for _, k := range sortedKeys(n.set.inputs) {
			v := n.set.inputs[k]
			if prev, ok := from[k]; ok {
				return nil, fmt.Errorf("%s: %s sets input %q of package %s, but %s already sets it at %s; an input has one source, so set it where the entrypoint imports package %s", n.block.DefRange, n.by, k, p.name, prev.by, prev.block.DefRange, p.name)
			}
			from[k] = n
			set.inputs[k] = v
		}
	}
	set.features = sortedKeys(on)
	return set, nil
}

// checkSatisfied reports what the entrypoint's import of p leaves unmet for
// another importer: a feature off, or an input with another value.
func checkSatisfied(p *pkgInstance, n pkgNeed) error {
	at := p.at
	if p.set != nil {
		at = p.set.at
	}
	for _, f := range n.set.features {
		if p.settings.features[f] {
			continue
		}
		return fmt.Errorf("%s: package %s runs with feature %q off, but %s needs it (%s):\n  %s: %s\nturn it on here with features = [%q], or %s", at, p.name, f, n.by, n.block.DefRange, f, p.file.block.features[f], f, dropHint(n))
	}
	for _, k := range sortedKeys(n.set.inputs) {
		want := n.set.inputs[k]
		got := p.settings.inputs[k]
		if got.RawEquals(want) {
			continue
		}
		return fmt.Errorf("%s: package %s runs with input %q = %s, but %s needs %s (%s); set inputs = { %s = %s } here", at, p.name, k, hclValue(got), n.by, hclValue(want), n.block.DefRange, k, hclValue(want))
	}
	return nil
}

func dropHint(n pkgNeed) string {
	if n.block.EnabledRange == (hcl.Range{}) {
		return "drop that import"
	}
	var names []string
	for _, trav := range n.block.Enabled.Variables() {
		if len(trav) < 2 {
			continue
		}
		if name, ok := traverseAttrName(trav[1]); ok && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return fmt.Sprintf("turn off what enables that import in %s (features %s)", n.by, strings.Join(names, ", "))
}

func hclValue(v cty.Value) string {
	if v == cty.NilVal {
		return "unset"
	}
	return strings.TrimSpace(string(hclwrite.TokensForValue(v).Bytes()))
}

// declaredInputs evaluates a package's inputs: each name to its default or
// to requiredSentinel.
func declaredInputs(pb *packageBlock, who string) (map[string]cty.Value, error) {
	declared := map[string]cty.Value{}
	if pb.InputsRange == (hcl.Range{}) {
		return declared, nil
	}
	ctx := staticEvalCtx()
	ctx.Variables = map[string]cty.Value{"required": cty.StringVal(requiredSentinel)}
	v, diags := pb.Inputs.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: inputs of %s: %s", pb.InputsRange, who, diags.Error())
	}
	if v.IsNull() {
		return declared, nil
	}
	if !v.Type().IsObjectType() && !v.Type().IsMapType() {
		return nil, fmt.Errorf("%s: inputs of %s must be an object such as { name = default }, got %s", pb.InputsRange, who, v.Type().FriendlyName())
	}
	maps.Copy(declared, v.AsValueMap())
	return declared, nil
}

func checkDeclared(pb *packageBlock, declared map[string]cty.Value, set *pkgSettings, who string) error {
	for _, n := range set.features {
		if _, ok := pb.features[n]; !ok {
			return fmt.Errorf("%s: feature %q is not declared by %s (declared: %s)", set.at, n, who, listOrNone(sortedKeys(pb.features)))
		}
	}
	for _, n := range sortedKeys(set.inputs) {
		if _, ok := declared[n]; !ok {
			return fmt.Errorf("%s: input %q is not declared by %s (declared: %s)", set.at, n, who, listOrNone(sortedKeys(declared)))
		}
	}
	return nil
}

// resolveSettings computes the inputs and features a package's modules
// see. set is what configures it; nil means a package run on its own:
// defaults and no features.
func resolveSettings(pb *packageBlock, set *pkgSettings, who string) (*scopeSettings, error) {
	declared, err := declaredInputs(pb, who)
	if err != nil {
		return nil, err
	}
	s := idleSettings(pb)
	if set != nil {
		if err := checkDeclared(pb, declared, set, who); err != nil {
			return nil, err
		}
		for _, n := range set.features {
			s.features[n] = true
		}
	}
	for _, name := range sortedKeys(declared) {
		if set != nil {
			if v, ok := set.inputs[name]; ok {
				s.inputs[name] = v
				continue
			}
		}
		def := declared[name]
		if !def.RawEquals(cty.StringVal(requiredSentinel)) {
			s.inputs[name] = def
			continue
		}
		if set != nil {
			return nil, fmt.Errorf("%s: %s needs input %q; pass it with inputs = { %s = ... }", set.at, who, name, name)
		}
		return nil, fmt.Errorf("%s: input %q of %s is required, so the package cannot run on its own", pb.InputsRange, name, who)
	}
	return s, nil
}

// idleSettings are every feature off and no inputs: what a package outside
// the stack sees, and where resolveSettings starts.
func idleSettings(pb *packageBlock) *scopeSettings {
	s := &scopeSettings{inputs: map[string]cty.Value{}, features: map[string]bool{}}
	for n := range pb.features {
		s.features[n] = false
	}
	return s
}

// evalEnabled evaluates an enabled attribute. It accepts feature
// expressions only (features.x, !features.x, &&, ||), so which blocks exist
// is known from the features alone, before any service is evaluated.
func evalEnabled(expr hcl.Expression, s *scopeSettings) (bool, error) {
	r := expr.Range()
	if s == nil || len(s.features) == 0 {
		return false, fmt.Errorf("%s: enabled needs a feature, and none is declared here; only a package declares features = { ... }", r)
	}
	if se, ok := expr.(hclsyntax.Expression); ok {
		var call hcl.Range
		hclsyntax.VisitAll(se, func(n hclsyntax.Node) hcl.Diagnostics {
			if fc, ok := n.(*hclsyntax.FunctionCallExpr); ok && call == (hcl.Range{}) {
				call = fc.Range()
			}
			return nil
		})
		if call != (hcl.Range{}) {
			return false, fmt.Errorf("%s: enabled accepts feature expressions only, such as features.x, !features.x or features.a && features.b; it cannot call functions", call)
		}
	}
	for _, trav := range expr.Variables() {
		if trav.RootName() != "features" || len(trav) < 2 {
			return false, fmt.Errorf("%s: enabled may reference features.<name> only", trav.SourceRange())
		}
		name, ok := traverseAttrName(trav[1])
		if _, declared := s.features[name]; !ok || !declared {
			return false, fmt.Errorf("%s: unknown feature %q (declared: %s)", trav.SourceRange(), name, listOrNone(sortedKeys(s.features)))
		}
	}
	vals := make(map[string]cty.Value, len(s.features))
	for n, on := range s.features {
		vals[n] = cty.BoolVal(on)
	}
	v, diags := expr.Value(&hcl.EvalContext{Variables: map[string]cty.Value{"features": cty.ObjectVal(vals)}})
	if diags.HasErrors() {
		return false, fmt.Errorf("%s: enabled: %s", r, diags.Error())
	}
	if v.IsNull() || !v.IsKnown() || !v.Type().Equals(cty.Bool) {
		return false, fmt.Errorf("%s: enabled must be true or false", r)
	}
	return v.True(), nil
}

func staticEvalCtx() *hcl.EvalContext {
	fns := encodeFuncs()
	fns["os::env"] = osEnvFunc()
	return &hcl.EvalContext{Functions: fns}
}

// offRecord remembers where a block was switched off, for the error that
// names a kept block still referencing it.
type offRecord struct {
	what string
	at   hcl.Range
}

type offIndex struct {
	services map[string]offRecord            // ServiceRef
	files    map[string]offRecord            // ServiceRef + "/" + file
	provs    map[string]offRecord            // ServiceRef + "/" + provision
	links    map[string]map[string]offRecord // scope → module or package
}

func (o *offIndex) empty() bool {
	return len(o.services) == 0 && len(o.files) == 0 && len(o.provs) == 0 && len(o.links) == 0
}

// prune drops the services, files, provisions and sudo steps whose enabled is false and
// stamps module onto what stays. Blocks without enabled keep their pointer.
func (t *Tree) prune(services []*serviceBlock, s *scopeSettings, module string) ([]*serviceBlock, error) {
	out := make([]*serviceBlock, 0, len(services))
	for _, sb := range services {
		if sb.EnabledRange != (hcl.Range{}) {
			on, err := evalEnabled(sb.Enabled, s)
			if err != nil {
				return nil, err
			}
			if !on {
				t.off().services[ServiceRef(module, sb.Toolchain, sb.Name)] = offRecord{what: fmt.Sprintf("service %q %q", sb.Toolchain, sb.Name), at: sb.EnabledRange}
				continue
			}
		}
		cp, err := t.pruneService(sb, s, module)
		if err != nil {
			return nil, err
		}
		if cp.module != module {
			if cp == sb {
				c := *sb
				cp = &c
			}
			cp.module = module
		}
		out = append(out, cp)
	}
	return out, nil
}

// pruneModule instantiates a module under its id: enabled applied, the id
// stamped on its services, and the package's toolchain as its pin when it
// has none of its own.
func (t *Tree) pruneModule(mb *moduleBlock, s *scopeSettings, id string, pkgToolchain *toolchainBlock) (*moduleBlock, error) {
	services, err := t.prune(mb.Services, s, id)
	if err != nil {
		return nil, err
	}
	cp := *mb
	cp.Name = id
	cp.Services = services
	if cp.Toolchain == nil {
		cp.Toolchain = pkgToolchain
	}
	return &cp, nil
}

func (t *Tree) pruneService(sb *serviceBlock, s *scopeSettings, module string) (*serviceBlock, error) {
	ref := ServiceRef(module, sb.Toolchain, sb.Name)
	var files []*fileBlock
	filesChanged := false
	for _, fb := range sb.Files {
		if fb.EnabledRange != (hcl.Range{}) {
			on, err := evalEnabled(fb.Enabled, s)
			if err != nil {
				return nil, err
			}
			if !on {
				filesChanged = true
				t.off().files[ref+"/"+fb.Name] = offRecord{what: fmt.Sprintf("file %q", fb.Name), at: fb.EnabledRange}
				t.disableNested(sb.Body, "", "file", fb.Name)
				continue
			}
		}
		files = append(files, fb)
	}
	var provs []*provisionBlock
	provsChanged := false
	if sb.Runtime != nil {
		for _, pb := range sb.Runtime.Provision {
			if pb.EnabledRange != (hcl.Range{}) {
				on, err := evalEnabled(pb.Enabled, s)
				if err != nil {
					return nil, err
				}
				if !on {
					provsChanged = true
					t.off().provs[ref+"/"+pb.Name] = offRecord{what: fmt.Sprintf("provision %q", pb.Name), at: pb.EnabledRange}
					t.disableNested(sb.Body, "runtime", "provision", pb.Name)
					continue
				}
			}
			provs = append(provs, pb)
		}
	}
	var sudos []*sudoBlock
	sudosChanged := false
	for _, sd := range sb.Sudo {
		if sd.EnabledRange != (hcl.Range{}) {
			on, err := evalEnabled(sd.Enabled, s)
			if err != nil {
				return nil, err
			}
			if !on {
				sudosChanged = true
				t.disableNested(sb.Body, "", "sudo", sd.Name)
				continue
			}
		}
		sudos = append(sudos, sd)
	}
	if !filesChanged && !provsChanged && !sudosChanged {
		return sb, nil
	}
	cp := *sb
	if sudosChanged {
		cp.Sudo = sudos
	}
	if filesChanged {
		cp.Files = files
	}
	if provsChanged {
		rt := *sb.Runtime
		rt.Provision = provs
		cp.Runtime = &rt
	}
	return &cp, nil
}

// disableNested records the source range of a nested block, optionally
// inside a parent block, so static passes over the service body skip it.
func (t *Tree) disableNested(body hcl.Body, parent, typ, label string) {
	sb, ok := body.(*hclsyntax.Body)
	if !ok {
		return
	}
	if parent != "" {
		for _, blk := range sb.Blocks {
			if blk.Type == parent {
				t.disableNested(blk.Body, "", typ, label)
			}
		}
		return
	}
	for _, blk := range sb.Blocks {
		if blk.Type == typ && len(blk.Labels) > 0 && blk.Labels[0] == label {
			t.disabled = append(t.disabled, blk.Range())
		}
	}
}

func (t *Tree) off() *offIndex {
	if t.offs == nil {
		t.offs = &offIndex{
			services: map[string]offRecord{},
			files:    map[string]offRecord{},
			provs:    map[string]offRecord{},
			links:    map[string]map[string]offRecord{},
		}
	}
	return t.offs
}

// checkGated reports a kept block that references something enabled
// switched off. Without this the reference would fail later as a bare
// "unsupported attribute" with no hint about the feature.
func (t *Tree) checkGated() error {
	if t.offs == nil || t.offs.empty() {
		return nil
	}
	services := slices.Clone(t.entryServices)
	for _, mb := range t.modules {
		services = append(services, mb.Services...)
	}
	for _, sb := range services {
		body, ok := sb.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		ref := ServiceRef(sb.module, sb.Toolchain, sb.Name)
		var found error
		hclsyntax.VisitAll(body, func(n hclsyntax.Node) hcl.Diagnostics {
			if found != nil || t.isDisabled(n.Range()) {
				return nil
			}
			st, ok := n.(*hclsyntax.ScopeTraversalExpr)
			if !ok {
				return nil
			}
			if rec, hit := t.gatedTarget(st.Traversal, sb.module, ref); hit {
				found = fmt.Errorf("%s: references %s, which is switched off by enabled at %s; give this block the same enabled", st.SrcRange, rec.what, rec.at)
			}
			return nil
		})
		if found != nil {
			return found
		}
	}
	return nil
}

func (t *Tree) gatedTarget(trav hcl.Traversal, module, selfRef string) (offRecord, bool) {
	names := make([]string, 0, len(trav))
	names = append(names, trav.RootName())
	for _, step := range trav[1:] {
		name, ok := traverseAttrName(step)
		if !ok {
			break
		}
		names = append(names, name)
	}
	o := t.offs
	switch {
	case names[0] == "self" && len(names) >= 3 && names[1] == "file":
		rec, ok := o.files[selfRef+"/"+names[2]]
		return rec, ok
	case names[0] == "self" && len(names) >= 4 && names[1] == "runtime" && names[2] == "provision":
		rec, ok := o.provs[selfRef+"/"+names[3]]
		return rec, ok
	case names[0] == "service" && len(names) >= 3:
		rec, ok := o.services[ServiceRef(module, names[1], names[2])]
		return rec, ok
	case names[0] == "module" && len(names) >= 2:
		id := resolveModule(module, names[1])
		if rec, ok := t.offLink(module, id); ok && !t.moduleVisible(module, id) {
			return rec, true
		}
		if len(names) >= 5 && names[2] == "service" {
			rec, ok := o.services[ServiceRef(id, names[3], names[4])]
			return rec, ok
		}
	case names[0] == "package" && len(names) >= 2:
		if rec, ok := t.offLink(module, pkgScope(names[1])); ok && !t.packageVisible(module, names[1]) {
			return rec, true
		}
		if len(names) >= 7 && names[2] == "module" && names[4] == "service" {
			rec, ok := o.services[ServiceRef(names[1]+"/"+names[3], names[5], names[6])]
			return rec, ok
		}
	}
	return offRecord{}, false
}

// offLink finds a switched-off import or require of target in a module's
// scope or in the scope of the package it belongs to.
func (t *Tree) offLink(module, target string) (offRecord, bool) {
	if rec, ok := t.offs.links[module][target]; ok {
		return rec, true
	}
	if p, ok := packageOf(module); ok {
		rec, ok := t.offs.links[pkgScope(p)][target]
		return rec, ok
	}
	return offRecord{}, false
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

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

	"github.com/piotrkowalczuk/zordon/internal/ztypes"
)

// pkgNeed is an import of a package from anywhere but the entrypoint's top
// level, with what it passes.
type pkgNeed struct {
	by    string // "package caddy" or "module gw"
	block *importBlock
	set   *pkgSettings
}

// inputDecl is one input of a package. A single input takes one value from
// one source; a many input takes entries from every provide block of its
// importers, keyed by provider.
type inputDecl struct {
	description string
	ty          ztypes.Type
	// def is the default value's expression; nil marks a required single
	// input.
	def    hcl.Expression
	many   bool
	unique []string
	// entry is ty as an object, for an input with many = true.
	entry ztypes.Object
}

// outputDecl is one output of a package.
type outputDecl struct {
	description string
	ty          ztypes.Type
	value       hcl.Expression
}

// inputArg is a value an import passes to one input, evaluated later, with
// the producers, in the scope of the import.
type inputArg struct {
	expr  hcl.Expression
	scope string
	at    hcl.Range
	src   string
}

// decodeAPI reads a package's features, inputs and outputs. Each is a map of
// name to { description = "...", ... }; types and value expressions are
// read from the syntax, so nothing is evaluated here.
func (pb *packageBlock) decodeAPI() error {
	var err error
	if pb.features, err = decodeFeatures(pb); err != nil {
		return err
	}
	if pb.inputs, err = decodeInputs(pb); err != nil {
		return err
	}
	pb.outputs, err = decodeOutputs(pb)
	return err
}

// declItems iterates `<attr> = { <name> = { <field> = <expr> ... } ... }`,
// checking names and that each entry is an object with a description.
func declItems(expr hcl.Expression, at hcl.Range, what, example, pkg string, each func(name string, at hcl.Range, fields map[string]hcl.Expression) error) error {
	if at == (hcl.Range{}) {
		return nil
	}
	shape := fmt.Sprintf("%ss of package %q map each name to { description = \"...\", ... }, such as %s", what, pkg, example)
	obj, ok := expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return fmt.Errorf("%s: %s", at, shape)
	}
	seen := map[string]bool{}
	for _, item := range obj.Items {
		name, ok := staticKey(item.KeyExpr)
		itemAt := item.KeyExpr.Range()
		if !ok || !moduleNameRe.MatchString(name) {
			return fmt.Errorf("%s: %s of package %q: name it with letters, digits, '_' or '-', starting with a letter", itemAt, what, pkg)
		}
		if seen[name] {
			return fmt.Errorf("%s: %s %q of package %q is declared twice", itemAt, what, name, pkg)
		}
		seen[name] = true
		body, ok := item.ValueExpr.(*hclsyntax.ObjectConsExpr)
		if !ok {
			return fmt.Errorf("%s: %s %q: %s", itemAt, what, name, shape)
		}
		fields := map[string]hcl.Expression{}
		for _, f := range body.Items {
			key, ok := staticKey(f.KeyExpr)
			if !ok {
				return fmt.Errorf("%s: %s %q: %s", f.KeyExpr.Range(), what, name, shape)
			}
			fields[key] = f.ValueExpr
		}
		d, ok := fields["description"]
		if !ok {
			return fmt.Errorf("%s: %s %q of package %q needs a description, so whoever uses the package knows what it is for", itemAt, what, name, pkg)
		}
		v, diags := d.Value(nil)
		if diags.HasErrors() || v.IsNull() || !v.Type().Equals(cty.String) || strings.TrimSpace(v.AsString()) == "" {
			return fmt.Errorf("%s: %s %q of package %q needs a description, so whoever uses the package knows what it is for", d.Range(), what, name, pkg)
		}
		if err := each(name, itemAt, fields); err != nil {
			return err
		}
	}
	return nil
}

func staticKey(expr hcl.Expression) (string, bool) {
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() || !v.Type().Equals(cty.String) {
		return "", false
	}
	return v.AsString(), true
}

func onlyFields(fields map[string]hcl.Expression, what, name string, at hcl.Range, allowed ...string) error {
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("%s: %s %q takes %s only", at, what, name, strings.Join(allowed, ", "))
		}
	}
	return nil
}

func decodeFeatures(pb *packageBlock) (map[string]string, error) {
	out := map[string]string{}
	err := declItems(pb.Features, pb.FeaturesRange, "feature", `{ tls = { description = "Serves HTTPS with a local CA." } }`, pb.Name, func(name string, at hcl.Range, fields map[string]hcl.Expression) error {
		if err := onlyFields(fields, "feature", name, at, "description"); err != nil {
			return err
		}
		v, _ := fields["description"].Value(nil)
		out[name] = v.AsString()
		return nil
	})
	return out, err
}

func decodeInputs(pb *packageBlock) (map[string]*inputDecl, error) {
	out := map[string]*inputDecl{}
	err := declItems(pb.Inputs, pb.InputsRange, "input", `{ port = { description = "Port to listen on.", type = number, default = null } }`, pb.Name, func(name string, at hcl.Range, fields map[string]hcl.Expression) error {
		if err := onlyFields(fields, "input", name, at, "description", "type", "default", "many", "unique"); err != nil {
			return err
		}
		d, _ := fields["description"].Value(nil)
		decl := &inputDecl{description: d.AsString(), def: fields["default"]}
		te, ok := fields["type"]
		if !ok {
			return fmt.Errorf("%s: input %q of package %q needs a type, such as type = string", at, name, pb.Name)
		}
		ty, err := ztypes.Parse(te)
		if err != nil {
			return fmt.Errorf("%s: input %q: type: %w", te.Range(), name, err)
		}
		decl.ty = ty
		if m, ok := fields["many"]; ok {
			v, diags := m.Value(nil)
			if diags.HasErrors() || v.IsNull() || !v.Type().Equals(cty.Bool) {
				return fmt.Errorf("%s: input %q: many must be true or false", m.Range(), name)
			}
			decl.many = v.True()
		}
		if u, ok := fields["unique"]; ok {
			if !decl.many {
				return fmt.Errorf("%s: input %q: unique applies to an input with many = true", u.Range(), name)
			}
			v, diags := u.Value(nil)
			if diags.HasErrors() || v.IsNull() || !(v.Type().IsTupleType() || v.Type().IsListType()) {
				return fmt.Errorf("%s: input %q: unique lists entry attributes, such as [\"host\"]", u.Range(), name)
			}
			for _, a := range v.AsValueSlice() {
				if a.IsNull() || !a.Type().Equals(cty.String) {
					return fmt.Errorf("%s: input %q: unique lists entry attributes, such as [\"host\"]", u.Range(), name)
				}
				decl.unique = append(decl.unique, a.AsString())
			}
		}
		if decl.many {
			entry, isObject := ty.(ztypes.Object)
			switch {
			case decl.def != nil:
				return fmt.Errorf("%s: input %q has many = true, so it starts empty and takes no default", decl.def.Range(), name)
			case !isObject:
				return fmt.Errorf("%s: input %q: an input with many = true takes entries from provide blocks, so its type must be object({ ... })", te.Range(), name)
			case entry.Has("key"):
				return fmt.Errorf("%s: input %q: the entry type cannot declare key; every entry has it, set to its key", te.Range(), name)
			}
			decl.entry = entry
			for _, a := range decl.unique {
				if !entry.Has(a) {
					return fmt.Errorf("%s: input %q: unique names %q, which the type does not declare", at, name, a)
				}
			}
		}
		out[name] = decl
		return nil
	})
	return out, err
}

func decodeOutputs(pb *packageBlock) (map[string]*outputDecl, error) {
	out := map[string]*outputDecl{}
	err := declItems(pb.Outputs, pb.OutputsRange, "output", `{ url = { description = "Where it listens.", value = "http://127.0.0.1:${module.web.service.go.web.vars.port}" } }`, pb.Name, func(name string, at hcl.Range, fields map[string]hcl.Expression) error {
		if err := onlyFields(fields, "output", name, at, "description", "type", "value"); err != nil {
			return err
		}
		value, ok := fields["value"]
		if !ok {
			return fmt.Errorf("%s: output %q of package %q needs a value", at, name, pb.Name)
		}
		te, ok := fields["type"]
		if !ok {
			return fmt.Errorf("%s: output %q of package %q needs a type, such as type = string", at, name, pb.Name)
		}
		ty, err := ztypes.Parse(te)
		if err != nil {
			return fmt.Errorf("%s: output %q: type: %w", te.Range(), name, err)
		}
		d, _ := fields["description"].Value(nil)
		out[name] = &outputDecl{description: d.AsString(), ty: ty, value: value}
		return nil
	})
	return out, err
}

// passedSettings reads what an import passes to a package: its features and,
// per input, the expression that gives the value, evaluated later in scope.
func passedSettings(ib *importBlock, scope string, src []byte) (*pkgSettings, error) {
	set := &pkgSettings{at: ib.DefRange, inputs: map[string]*inputArg{}, features: slices.Clone(ib.Features)}
	sort.Strings(set.features)
	for i := 1; i < len(set.features); i++ {
		if set.features[i] == set.features[i-1] {
			return nil, fmt.Errorf("%s: import %q lists feature %q twice", ib.DefRange, ib.Path, set.features[i])
		}
	}
	if ib.InputsRange == (hcl.Range{}) {
		return set, nil
	}
	obj, ok := ib.Inputs.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return nil, fmt.Errorf("%s: import %q: inputs must be an object such as { name = value }", ib.InputsRange, ib.Path)
	}
	for _, item := range obj.Items {
		name, ok := staticKey(item.KeyExpr)
		if !ok {
			return nil, fmt.Errorf("%s: import %q: inputs must be an object such as { name = value }", item.KeyExpr.Range(), ib.Path)
		}
		arg := &inputArg{expr: item.ValueExpr, scope: scope, at: item.ValueExpr.Range()}
		if r := item.ValueExpr.Range(); src != nil && r.End.Byte <= len(src) {
			arg.src = string(r.SliceBytes(src))
		}
		set.inputs[name] = arg
	}
	return set, nil
}

func sameSettings(a, b *pkgSettings) bool {
	if !slices.Equal(a.features, b.features) || len(a.inputs) != len(b.inputs) {
		return false
	}
	for k, av := range a.inputs {
		bv, ok := b.inputs[k]
		if !ok || av.src != bv.src {
			return false
		}
	}
	return true
}

// resolvePackages fixes the features of every package and the source of each
// of its inputs. A package the entrypoint imports at its top level gets what
// that import passes, and every other import of it in the stack must be
// satisfied by that. Any other package gets the union of the features its
// importers in the stack pass, and each input from the one importer that
// sets it; importers are resolved first, because their features decide which
// of their imports count.
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
		p.settings, err = resolveSettings(p, pb, p.set, who)
		return err
	}
	needs, err := t.needsOf(p, plain)
	if err != nil {
		return err
	}
	if len(needs) == 0 {
		p.settings = idleSettings(pb)
		return nil
	}
	p.active, p.needs = true, needs
	set, err := mergeNeeds(p, needs)
	if err != nil {
		return err
	}
	p.settings, err = resolveSettings(p, pb, set, who)
	return err
}

// needsOf lists the imports of p that are part of the stack, other than the
// entrypoint's top level: from scopes outside packages the entrypoint
// reaches, and from packages in the stack whose enabled keeps the import.
func (t *Tree) needsOf(p *pkgInstance, plain map[string]bool) ([]pkgNeed, error) {
	who := "package " + p.name
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
				by = "package " + owner
			} else if !plain[scope] {
				continue
			}
			set, err := passedSettings(l.block, scope, nil)
			if err != nil {
				return nil, err
			}
			if err := checkDeclared(p.file.block, set, who); err != nil {
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
	set := &pkgSettings{at: needs[0].block.DefRange, inputs: map[string]*inputArg{}}
	from := map[string]pkgNeed{}
	on := map[string]bool{}
	for _, n := range needs {
		for _, f := range n.set.features {
			on[f] = true
		}
		for _, k := range sortedKeys(n.set.inputs) {
			if prev, ok := from[k]; ok {
				return nil, fmt.Errorf("%s: %s sets input %q of package %s, but %s already sets it at %s; an input has one source, so set it where the entrypoint imports package %s", n.block.DefRange, n.by, k, p.name, prev.by, prev.block.DefRange, p.name)
			}
			from[k] = n
			set.inputs[k] = n.set.inputs[k]
		}
	}
	set.features = sortedKeys(on)
	return set, nil
}

// checkSatisfied reports a feature another importer needs and the
// entrypoint's import of p leaves off. An input it sets is compared with the
// value p runs with once both are evaluated.
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
		p.checks = append(p.checks, &inputCheck{name: k, arg: n.set.inputs[k], by: n.by, at: at})
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

func checkDeclared(pb *packageBlock, set *pkgSettings, who string) error {
	for _, n := range set.features {
		if _, ok := pb.features[n]; !ok {
			return fmt.Errorf("%s: feature %q is not declared by %s (declared: %s)", set.at, n, who, listOrNone(sortedKeys(pb.features)))
		}
	}
	for _, n := range sortedKeys(set.inputs) {
		decl, ok := pb.inputs[n]
		if !ok {
			return fmt.Errorf("%s: input %q is not declared by %s (declared: %s)", set.at, n, who, listOrNone(sortedKeys(pb.inputs)))
		}
		if decl.many {
			return fmt.Errorf("%s: input %q of %s has many = true, so importers add entries with provide %q {} instead of setting it", set.inputs[n].at, n, who, n)
		}
	}
	return nil
}

// resolveSettings fixes the features a package runs with and the source of
// each of its single inputs: what configures it, else the default. set nil
// means a package run on its own: defaults and no features.
func resolveSettings(p *pkgInstance, pb *packageBlock, set *pkgSettings, who string) (*scopeSettings, error) {
	s := idleSettings(pb)
	p.inputs = map[string]*inputArg{}
	if set != nil {
		if err := checkDeclared(pb, set, who); err != nil {
			return nil, err
		}
		for _, n := range set.features {
			s.features[n] = true
		}
		maps.Copy(p.inputs, set.inputs)
	}
	for _, name := range sortedKeys(pb.inputs) {
		decl := pb.inputs[name]
		if decl.many || p.inputs[name] != nil || decl.def != nil {
			continue
		}
		if set != nil {
			return nil, fmt.Errorf("%s: %s needs input %q; pass it with inputs = { %s = ... }", set.at, who, name, name)
		}
		return nil, fmt.Errorf("%s: input %q of %s is required, so the package cannot run on its own", pb.InputsRange, name, who)
	}
	return s, nil
}

// idleSettings are every feature off: what a package outside the stack sees,
// and where resolveSettings starts.
func idleSettings(pb *packageBlock) *scopeSettings {
	s := &scopeSettings{features: map[string]bool{}}
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

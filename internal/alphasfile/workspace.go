package alphasfile

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zdoc"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// DefaultBranchTemplate is the branch a picked service's checkout lives on
// when the manifest does not override it. Per-service by construction, which
// is what stops two services (or two workspaces) colliding on one branch.
const DefaultBranchTemplate = "zordon/${workspace.name}/${service.name}"

// DefaultBranchFor renders DefaultBranchTemplate directly. It is the fallback
// for a resolved service that carries no WorkspaceBranch — an Alphasfile
// resolved before this field existed, or a federation parent that predates it.
func DefaultBranchFor(workspace, service string) string {
	return "zordon/" + workspace + "/" + service
}

// WorkspaceFileOp is which of the three ownership models a file uses.
type WorkspaceFileOp string

const (
	// OpCreate writes a whole file zordon owns.
	OpCreate WorkspaceFileOp = "create"
	// OpMerge owns a fragment of a structured file somebody else owns.
	OpMerge WorkspaceFileOp = "merge"
	// OpRegion owns a comment-delimited span of a text file.
	OpRegion WorkspaceFileOp = "region"
)

// WorkspaceFile is one resolved entry of the top-level `workspace {}` block.
// Path is still manifest-relative: resolving it against the workspace dir,
// and refusing the ones that would land in zordon's own state, is the
// writer's job.
type WorkspaceFile struct {
	Name    string
	Path    string
	Op      WorkspaceFileOp
	Body    string      // create, region
	Data    any         // merge
	Format  zdoc.Format // merge
	Comment string      // region
}

// WorkspaceSpec is a resolved top-level `workspace {}` block.
type WorkspaceSpec struct {
	Files []*WorkspaceFile

	branch         hcl.Expression // nil ⇒ the manifest set none, use the default
	branchTemplate string
	inv            *invocation.InvocationState
}

// RenderWorkspace resolves the top-level `workspace {}` block for one
// workspace. It is a parse-only read of the manifest — services are not
// evaluated, nothing is cloned, no process runs — because it is called by
// `zordon workspace create` before a stack exists at all.
//
// That timing is also why the evaluation context is deliberately narrow. See
// workspaceCtx: anything whose value is only known once alpha is running is
// registered as an error rather than left to resolve into something wrong.
//
// A manifest with no `workspace {}` block yields no files and the default
// branch template, so every existing Alphasfile keeps working untouched.
func RenderWorkspace(path string, inv *invocation.InvocationState) (*WorkspaceSpec, error) {
	if inv == nil {
		panic("alphasfile: RenderWorkspace requires an invocation")
	}
	b, err := zfs.Read(path)
	if err != nil {
		return nil, fmt.Errorf("alphasfile read: %w", err)
	}
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(b, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("alphasfile parse: %s", diags.Error())
	}
	var root rootBlock
	if diags := gohcl.DecodeBody(file.Body, nil, &root); diags.HasErrors() {
		return nil, fmt.Errorf("alphasfile decode: %s", diags.Error())
	}

	spec := &WorkspaceSpec{inv: inv, branchTemplate: DefaultBranchTemplate}
	if root.Workspace == nil {
		return spec, nil
	}
	if attrSet(root.Workspace.Branch) {
		spec.branch = root.Workspace.Branch
		spec.branchTemplate = string(root.Workspace.Branch.Range().SliceBytes(b))
	}

	ctx := workspaceCtx(inv)
	seen := make(map[string]bool, len(root.Workspace.Files))
	for _, fb := range root.Workspace.Files {
		if seen[fb.Name] {
			return nil, fmt.Errorf("workspace.file %q: declared twice", fb.Name)
		}
		seen[fb.Name] = true
		wf, err := resolveWorkspaceFile(fb, ctx, inv)
		if err != nil {
			return nil, fmt.Errorf("workspace.file %q: %w", fb.Name, err)
		}
		spec.Files = append(spec.Files, wf)
	}
	return spec, nil
}

// BranchFor renders the branch template for one service.
func (s *WorkspaceSpec) BranchFor(service, toolchain string) (string, error) {
	return renderBranch(s.branch, s.inv, service, toolchain)
}

// BranchesFor renders every service's branch and rejects a template that maps
// two services onto one name — a constant template, say. Per-service branches
// are precisely what keeps parallel checkouts from colliding, so a collision
// has to be a manifest error rather than a git failure much later.
func (s *WorkspaceSpec) BranchesFor(metas []*ServiceMeta) (map[string]string, error) {
	out := make(map[string]string, len(metas))
	for _, m := range metas {
		br, err := s.BranchFor(m.Name, m.Toolchain)
		if err != nil {
			return nil, err
		}
		out[m.Name] = br
	}
	if err := checkBranchesUnique(out); err != nil {
		return nil, err
	}
	return out, nil
}

// BranchTemplate reports the manifest's template source, for `zordon plan`.
func (s *WorkspaceSpec) BranchTemplate() string {
	if s == nil || s.branchTemplate == "" {
		return DefaultBranchTemplate
	}
	return s.branchTemplate
}

func resolveWorkspaceFile(fb *workspaceFileBlock, ctx *hcl.EvalContext, inv *invocation.InvocationState) (*WorkspaceFile, error) {
	ops := 0
	for _, set := range []bool{fb.Create != nil, fb.Merge != nil, fb.Region != nil} {
		if set {
			ops++
		}
	}
	if ops != 1 {
		return nil, fmt.Errorf("needs exactly one of create/merge/region, got %d", ops)
	}

	path, err := evalWorkspaceStr(fb.Path, ctx, "path")
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("path is empty")
	}
	wf := &WorkspaceFile{Name: fb.Name, Path: path}

	switch {
	case fb.Create != nil:
		wf.Op = OpCreate
		wf.Body, err = evalContent(fb.Create.Body, fb.Create.Source, ctx, inv, "create")
		if err != nil {
			return nil, err
		}
	case fb.Region != nil:
		wf.Op = OpRegion
		if f, ok := zdoc.FormatOf(path); ok && f == zdoc.FormatJSON {
			return nil, errors.New("region cannot be used on a JSON file (no comment syntax); use merge instead")
		}
		wf.Comment = fb.Region.Comment
		wf.Body, err = evalContent(fb.Region.Body, fb.Region.Source, ctx, inv, "region")
		if err != nil {
			return nil, err
		}
	case fb.Merge != nil:
		wf.Op = OpMerge
		if wf.Format, err = mergeFormat(fb.Merge.Format, path); err != nil {
			return nil, err
		}
		if err := rejectRuntimeRefs(fb.Merge.Data, "merge.data"); err != nil {
			return nil, err
		}
		v, diags := fb.Merge.Data.Value(ctx)
		if diags.HasErrors() {
			return nil, fmt.Errorf("merge.data: %s", diags.Error())
		}
		if wf.Data, err = ctyToGo(v); err != nil {
			return nil, fmt.Errorf("merge.data: %w", err)
		}
		if _, ok := wf.Data.(map[string]any); !ok {
			return nil, fmt.Errorf("merge.data must be an object, got %T", wf.Data)
		}
	}
	return wf, nil
}

// evalContent resolves a body/source pair into the text to write. source is
// read relative to the project root and may not escape it.
func evalContent(body, source hcl.Expression, ctx *hcl.EvalContext, inv *invocation.InvocationState, op string) (string, error) {
	hasBody, hasSource := attrSet(body), attrSet(source)
	switch {
	case hasBody && hasSource:
		return "", fmt.Errorf("%s: set body or source, not both", op)
	case !hasBody && !hasSource:
		return "", fmt.Errorf("%s: needs body or source", op)
	case hasBody:
		return evalWorkspaceStr(body, ctx, op+".body")
	}

	rel, err := evalWorkspaceStr(source, ctx, op+".source")
	if err != nil {
		return "", err
	}
	root := inv.ProjectRoot()
	abs, ok := zfs.Resolve(root, rel)
	if !ok {
		return "", fmt.Errorf("%s.source %q: must be a relative path inside the project root %s", op, rel, root)
	}
	b, err := zfs.Read(abs)
	if err != nil {
		return "", fmt.Errorf("%s.source %q: %w", op, rel, err)
	}
	// The file is an HCL template, not a verbatim copy, so it can reach
	// workspace.* the same way an inline body can. A literal ${ or %{ that is
	// NOT meant as interpolation has to be escaped $${ / %%{ — the usual HCL
	// rule, and the price of templating a whole file.
	tpl, diags := hclsyntax.ParseTemplate(b, abs, hcl.InitialPos)
	if diags.HasErrors() {
		return "", fmt.Errorf("%s.source %q: %s", op, rel, diags.Error())
	}
	if err := rejectRuntimeRefs(tpl, op+".source "+rel); err != nil {
		return "", err
	}
	v, diags := tpl.Value(ctx)
	if diags.HasErrors() {
		return "", fmt.Errorf("%s.source %q: %s", op, rel, diags.Error())
	}
	if v.IsNull() || v.Type() != cty.String {
		return "", fmt.Errorf("%s.source %q did not render to text", op, rel)
	}
	return v.AsString(), nil
}

// attrSet reports whether an optional hcl.Expression attribute was actually
// written. Checking for nil is not enough: gohcl fills an absent optional
// expression with a static null placeholder, so the field is non-nil either
// way. A placeholder evaluates to null against an empty context without
// complaint; a real expression either yields a value or complains about the
// variables that context lacks.
func attrSet(expr hcl.Expression) bool {
	if expr == nil {
		return false
	}
	v, diags := expr.Value(nil)
	return diags.HasErrors() || !v.IsNull()
}

func evalWorkspaceStr(expr hcl.Expression, ctx *hcl.EvalContext, field string) (string, error) {
	if expr == nil {
		return "", nil
	}
	if err := rejectRuntimeRefs(expr, field); err != nil {
		return "", err
	}
	v, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return "", fmt.Errorf("%s: %s", field, diags.Error())
	}
	if v.IsNull() {
		return "", nil
	}
	if v.Type() != cty.String {
		return "", fmt.Errorf("%s must be a string, got %s", field, v.Type().FriendlyName())
	}
	return v.AsString(), nil
}

func mergeFormat(declared, path string) (zdoc.Format, error) {
	if declared == "" {
		f, ok := zdoc.FormatOf(path)
		if !ok {
			return "", fmt.Errorf("merge: cannot tell the format of %q; set format = one of %s", path, strings.Join(zdoc.Formats(), ", "))
		}
		return f, nil
	}
	if slices.Contains(zdoc.Formats(), declared) {
		return zdoc.Format(declared), nil
	}
	return "", fmt.Errorf("merge: unknown format %q, want one of %s", declared, strings.Join(zdoc.Formats(), ", "))
}

// workspaceCtx is the evaluation context for the top-level workspace block.
//
// What it offers is everything knowable from the manifest and the directory
// alone. What it withholds is everything that only becomes true once alpha is
// running — and it withholds it LOUDLY, via stub functions that explain
// themselves, because the silent alternative (a port that does not match the
// one alpha later draws) produces a file that looks right and is wrong.
func workspaceCtx(inv *invocation.InvocationState) *hcl.EvalContext {
	fns := map[string]function.Function{
		"os::env":   osEnvFunc(),
		"fs::tmp":   staticStrFunc(inv.TmpDir),
		"fs::state": staticStrFunc(inv.StateDir),
		"fs::bin":   staticStrFunc(inv.BinDir()),
		"fs::hash":  staticStrFunc(inv.FsHash),
	}
	maps.Copy(fns, encodeFuncs())
	for _, name := range runtimeOnlyFuncs {
		fns[name] = unavailableFunc(name)
	}
	return &hcl.EvalContext{
		Functions: fns,
		Variables: map[string]cty.Value{
			"workspace": cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal(workspaceName(inv)),
				"dir":  cty.StringVal(inv.Dir),
				"root": cty.StringVal(inv.ProjectRoot()),
				"hash": cty.StringVal(inv.FsHash),
				"port": cty.NumberIntVal(int64(WorkspacePort(inv.FsHash))),
			}),
		},
	}
}

// workspaceName is inv.Workspace, falling back to the last element of
// StateDir. Struct-literal invocations leave Workspace empty while StateDir
// already ends in workspaces/<name>, so when the two disagree the directory is
// the one carrying real information.
func workspaceName(inv *invocation.InvocationState) string {
	if inv.Workspace != "" {
		return inv.Workspace
	}
	if inv.StateDir != "" {
		return filepath.Base(inv.StateDir)
	}
	return invocation.MainWorkspace
}

// runtimeOnlyFuncs cannot answer truthfully before the stack exists: either
// they need a service scope (fs::src and friends) or they would answer
// differently every time they are asked (net::pickport).
var runtimeOnlyFuncs = []string{
	"net::pickport",
	"src::hash",
	"cfg::hash",
	"fs::src",
	"fs::exe",
	"fs::etc",
	"fs::var",
	"fs::service::bin",
	"fs::service::etc",
	"fs::service::var",
	"fs::toolchain::bin",
	"env::prepend",
	"env::append",
	"tpl::render::flags",
	"test::log",
	"test::fail",
}

func unavailableFunc(name string) function.Function {
	return function.New(&function.Spec{
		VarParam: &function.Parameter{
			Name:             "args",
			Type:             cty.DynamicPseudoType,
			AllowNull:        true,
			AllowDynamicType: true,
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func([]cty.Value, cty.Type) (cty.Value, error) {
			return cty.NilVal, fmt.Errorf(
				"%s is not available in the top-level workspace block: these files are written by `zordon workspace create` / `workspace apply`, before any service is built or started. Put values that depend on a running stack in that service's own file{} block instead", name)
		},
	})
}

func staticStrFunc(v string) function.Function {
	return function.New(&function.Spec{
		Type: function.StaticReturnType(cty.String),
		Impl: func([]cty.Value, cty.Type) (cty.Value, error) { return cty.StringVal(v), nil },
	})
}

// rejectRuntimeRefs turns a reference to the dynamic namespace into an error
// that names the alternative. HCL's own "unknown variable" message would be
// accurate but would not explain why `service.go.api.vars.port` — legal three
// lines away in a service block — is refused here.
func rejectRuntimeRefs(expr hcl.Expression, field string) error {
	if expr == nil {
		return nil
	}
	for _, tr := range expr.Variables() {
		switch tr.RootName() {
		case "self":
			return fmt.Errorf("%s: `self` refers to a service and has no meaning in the top-level workspace block", field)
		case "service":
			return fmt.Errorf(
				"%s: a service's resolved values (like a picked port) do not exist yet when workspace files are written; declare a file{} block inside that service instead", field)
		case "toolchain", "never":
			return fmt.Errorf("%s: `%s` is not available in the top-level workspace block", field, tr.RootName())
		}
	}
	return nil
}

// resolveWorkspaceBranches renders the per-workspace branch for every service
// that can have a checkout, then rejects a template that would put two of them
// on the same branch.
//
// Only Editable services keep the result: an unpicked git service is cloned at
// its ref with no branch at all. The others are still rendered because
// validating the template is worth doing wherever the manifest is resolved —
// including `zordon plan` in main, where nothing is editable.
func (r *resolver) resolveWorkspaceBranches() error {
	if r.inv == nil {
		return nil // no invocation to name a workspace after
	}
	var branchExpr hcl.Expression
	if r.root != nil && r.root.Workspace != nil {
		branchExpr = r.root.Workspace.Branch
	}
	rendered := map[string]string{}
	for _, s := range r.resolvedServices {
		if s == nil || s.Package == nil {
			continue
		}
		if s.Package.Git == "" && s.Package.Src == "" {
			continue // use-only or pkg: nothing is ever checked out
		}
		br, err := renderBranch(branchExpr, r.inv, s.Name(), s.Toolchain)
		if err != nil {
			return err
		}
		rendered[s.Name()] = br
	}
	if err := checkBranchesUnique(rendered); err != nil {
		return err
	}
	for _, s := range r.resolvedServices {
		if s != nil && s.Package != nil && s.Package.Editable {
			s.Package.WorkspaceBranch = rendered[s.Name()]
		}
	}
	return nil
}

// renderBranch evaluates the branch template for one service. A manifest that
// set no template gets DefaultBranchTemplate; one that set an empty string
// gets a validation error, because that is a mistake rather than a default.
func renderBranch(expr hcl.Expression, inv *invocation.InvocationState, service, toolchain string) (string, error) {
	ctx := workspaceCtx(inv)
	ctx.Variables["service"] = cty.ObjectVal(map[string]cty.Value{
		"name":      cty.StringVal(service),
		"toolchain": cty.StringVal(toolchain),
	})
	if expr == nil || !attrSet(expr) {
		var err error
		if expr, err = defaultBranchExpr(); err != nil {
			return "", err
		}
	}
	v, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return "", fmt.Errorf("workspace.branch: %s", diags.Error())
	}
	if v.IsNull() || v.Type() != cty.String {
		return "", fmt.Errorf("workspace.branch must be a string, got %s", v.Type().FriendlyName())
	}
	br := v.AsString()
	if err := validateBranchName(br); err != nil {
		return "", fmt.Errorf("workspace.branch resolved to %q for service %q: %w", br, service, err)
	}
	return br, nil
}

func defaultBranchExpr() (hcl.Expression, error) {
	expr, diags := hclsyntax.ParseTemplate([]byte(DefaultBranchTemplate), "<default branch>", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("default branch template: %s", diags.Error())
	}
	return expr, nil
}

func checkBranchesUnique(byService map[string]string) error {
	owners := map[string][]string{}
	for svc, br := range byService {
		owners[br] = append(owners[br], svc)
	}
	for br, svcs := range owners {
		if len(svcs) < 2 {
			continue
		}
		sort.Strings(svcs)
		return fmt.Errorf(
			"workspace.branch renders to %q for more than one service (%s); include ${service.name} so each checkout gets its own branch",
			br, strings.Join(svcs, ", "))
	}
	return nil
}

// validateBranchName applies the subset of git check-ref-format that a
// template can plausibly violate, so a bad template fails in zordon with a
// readable message instead of inside a git subprocess.
func validateBranchName(b string) error {
	switch {
	case b == "":
		return errors.New("branch name is empty")
	case strings.HasPrefix(b, "/"), strings.HasSuffix(b, "/"):
		return errors.New("branch name may not start or end with '/'")
	case strings.Contains(b, "//"):
		return errors.New("branch name may not contain '//'")
	case strings.Contains(b, ".."):
		return errors.New("branch name may not contain '..'")
	case strings.HasSuffix(b, "."), strings.HasSuffix(b, ".lock"):
		return errors.New("branch name may not end with '.' or '.lock'")
	case b == "@":
		return errors.New("branch name may not be '@'")
	case strings.ContainsAny(b, " ~^:?*[\\\x7f"):
		return errors.New("branch name may not contain a space or any of ~^:?*[\\")
	}
	for _, r := range b {
		if r < 0x20 {
			return errors.New("branch name may not contain control characters")
		}
	}
	for seg := range strings.SplitSeq(b, "/") {
		if seg == "" {
			return errors.New("branch name has an empty path segment")
		}
		if strings.HasPrefix(seg, ".") {
			return fmt.Errorf("branch name segment %q may not start with '.'", seg)
		}
		if strings.HasSuffix(seg, ".lock") {
			return fmt.Errorf("branch name segment %q may not end with '.lock'", seg)
		}
	}
	return nil
}

// WorkspacePort derives a stable TCP port from a workspace's FsHash. Stable
// is the whole point: a generated file can carry the port of a server that is
// started later, which net::pickport (a fresh draw every evaluation) cannot
// do. The range avoids both privileged ports and the usual ephemeral range.
func WorkspacePort(fsHash string) int {
	const (
		base = 20000
		span = 20000
	)
	var sum uint32
	for _, c := range []byte(fsHash) {
		sum = sum*31 + uint32(c)
	}
	return base + int(sum%span)
}

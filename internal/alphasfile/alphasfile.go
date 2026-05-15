// Package alphasfile parses an Alphasfile (HCL2) into a serializable set of
// service definitions consumed by zordon and alpha.
//
// The runtime side (process spawn, install, etc.) lives elsewhere; here we
// only own the wire-stable data model plus the eval pipeline that resolves
// dynamic expressions (cross-service references, helpers like net.pickport)
// into concrete values before the data is shipped to alpha.
package alphasfile

import (
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"

	"github.com/piotrkowalczuk/zordon/internal/probe"
)

const (
	ToolchainGo   = "go"
	ToolchainRust = "rust"
	ToolchainRuby = "ruby"
)

// --- wire-stable types (JSON over the control socket) ---------------------

type LogConfig struct {
	Format string `json:"format,omitempty"`
	Filter string `json:"filter,omitempty"`
}

type RuntimeConfig struct {
	Name           string         `json:"name"`
	Color          string         `json:"color,omitempty"`
	Log            *LogConfig     `json:"log,omitempty"`
	DoubleDash     bool           `json:"double_dash,omitempty"`
	SpaceSeparated bool           `json:"space_separated,omitempty"`
	Vars           map[string]any `json:"vars,omitempty"`
	Arguments      map[string]any `json:"arguments,omitempty"`
	Command        []string       `json:"command,omitempty"`
	Sudo           []*SudoStep    `json:"sudo,omitempty"`
	Files          []*File        `json:"files,omitempty"`
	Dir            string         `json:"dir,omitempty"`
	Readiness      *probe.Probe   `json:"readiness,omitempty"`
}

type ServiceGo struct {
	Name    string `json:"name"`
	Import  string `json:"import"`
	Branch  string `json:"branch,omitempty"`
	Install string `json:"install,omitempty"`
}

type ServiceRust struct {
	Name        string   `json:"name"`
	Crate       string   `json:"crate,omitempty"`
	Version     string   `json:"version,omitempty"`
	Git         string   `json:"git,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	Tag         string   `json:"tag,omitempty"`
	Rev         string   `json:"rev,omitempty"`
	Bin         string   `json:"bin,omitempty"`
	Features    []string `json:"features,omitempty"`
	AllFeatures bool     `json:"all_features,omitempty"`
	Locked      bool     `json:"locked,omitempty"`
	Install     string   `json:"install,omitempty"`
}

type ServiceRuby struct {
	Name    string `json:"name"`
	Git     string `json:"git,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Rev     string `json:"rev,omitempty"`
	Install string `json:"install,omitempty"`
	Run     string `json:"run,omitempty"`
}

type Service struct {
	Toolchain string         `json:"toolchain"`
	Runtime   *RuntimeConfig `json:"runtime"`
	Go        *ServiceGo     `json:"go,omitempty"`
	Rust      *ServiceRust   `json:"rust,omitempty"`
	Ruby      *ServiceRuby   `json:"ruby,omitempty"`
}

func (s *Service) Name() string {
	switch {
	case s.Go != nil:
		return s.Go.Name
	case s.Rust != nil:
		return s.Rust.Name
	case s.Ruby != nil:
		return s.Ruby.Name
	}
	return ""
}

func (s *Service) NeedsSource() bool {
	switch {
	case s.Go != nil:
		return s.Go.Import != ""
	case s.Rust != nil:
		return s.Rust.Git != ""
	case s.Ruby != nil:
		return s.Ruby.Git != ""
	}
	return false
}

func (s *Service) Source() string {
	switch {
	case s.Go != nil:
		return s.Go.Import
	case s.Rust != nil:
		return s.Rust.Git
	case s.Ruby != nil:
		return s.Ruby.Git
	}
	return ""
}

func (s *Service) Branch() string {
	switch {
	case s.Go != nil:
		return s.Go.Branch
	case s.Rust != nil:
		switch {
		case s.Rust.Tag != "":
			return s.Rust.Tag
		case s.Rust.Rev != "":
			return s.Rust.Rev
		}
		return s.Rust.Branch
	case s.Ruby != nil:
		switch {
		case s.Ruby.Tag != "":
			return s.Ruby.Tag
		case s.Ruby.Rev != "":
			return s.Ruby.Rev
		}
		return s.Ruby.Branch
	}
	return ""
}

func (s *Service) Install() string {
	switch {
	case s.Go != nil:
		return s.Go.Install
	case s.Rust != nil:
		return s.Rust.Install
	case s.Ruby != nil:
		return s.Ruby.Install
	}
	return ""
}

// Flags renders RuntimeConfig.Arguments into argv flags. Format follows
// DoubleDash / SpaceSeparated; Ruby is always space-separated.
func (s *Service) Flags() []string {
	if s.Runtime == nil {
		return nil
	}
	spaceSep := s.Runtime.SpaceSeparated || s.Toolchain == ToolchainRuby
	out := make([]string, 0, 2*len(s.Runtime.Arguments))
	for flag, value := range s.Runtime.Arguments {
		switch {
		case spaceSep && s.Runtime.DoubleDash:
			out = append(out, fmt.Sprintf("--%s", flag), fmt.Sprintf("%v", value))
		case spaceSep:
			out = append(out, fmt.Sprintf("-%s", flag), fmt.Sprintf("%v", value))
		case s.Runtime.DoubleDash:
			out = append(out, fmt.Sprintf("--%s=%v", flag, value))
		default:
			out = append(out, fmt.Sprintf("-%s=%v", flag, value))
		}
	}
	return out
}

// File is a generated file written by alpha at Configure time and unlinked
// on shutdown. Path and Body are already-resolved concrete strings.
type File struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Body string `json:"body"`
}

// SudoStep is one resolved idempotent privileged hook. Check/Apply/Verify
// are concrete shell snippets. zordon runs Check unprivileged; if it exits
// non-zero (or is empty) it runs Apply via sudo, then optional Verify
// unprivileged.
type SudoStep struct {
	Name   string `json:"name"`
	Check  string `json:"check,omitempty"`
	Apply  string `json:"apply"`
	Verify string `json:"verify,omitempty"`
}

type Alphasfile struct {
	Services []*Service `json:"services,omitempty"`
}

func (af *Alphasfile) All() []*Service { return af.Services }

// AllFiles flattens every service's generated files into one list (the order
// services were resolved). Used by alpha to materialize / clean up.
func (af *Alphasfile) AllFiles() []*File {
	var out []*File
	for _, s := range af.Services {
		if s.Runtime != nil {
			out = append(out, s.Runtime.Files...)
		}
	}
	return out
}

// --- HCL2 intermediate (parser-internal) ----------------------------------

type rootBlock struct {
	Services []*serviceBlock `hcl:"service,block"`
}

type serviceBlock struct {
	Toolchain string `hcl:"toolchain,label"`
	Name      string `hcl:"name,label"`

	// static fields
	Color          string    `hcl:"color,optional"`
	Log            *logBlock `hcl:"log,block"`
	DoubleDash     bool      `hcl:"doubleDash,optional"`
	SpaceSeparated bool      `hcl:"space_separated,optional"`
	Dir            string    `hcl:"dir,optional"`
	Import         string    `hcl:"import,optional"`
	Branch         string    `hcl:"branch,optional"`
	Install        string    `hcl:"install,optional"`
	Crate          string    `hcl:"crate,optional"`
	Version        string    `hcl:"version,optional"`
	Git            string    `hcl:"git,optional"`
	Tag            string    `hcl:"tag,optional"`
	Rev            string    `hcl:"rev,optional"`
	Bin            string    `hcl:"bin,optional"`
	Features       []string  `hcl:"features,optional"`
	AllFeatures    bool      `hcl:"all_features,optional"`
	Locked         bool      `hcl:"locked,optional"`
	Run            string    `hcl:"run,optional"`

	// dynamic (DAG-evaluated, in this order:
	//   vars → files → arguments → command → readiness.port)
	Vars      hcl.Expression `hcl:"vars,optional"`
	Arguments hcl.Expression `hcl:"arguments,optional"`
	// Command, when set, is the full argv (program + args) and replaces the
	// default "<name> <flags>" invocation. Needed for binaries driven by a
	// subcommand (e.g. `caddy run --config ...`). Evaluated as a list of
	// strings, so it can interpolate self.vars / self.file / parent refs.
	Command hcl.Expression `hcl:"command,optional"`
	// Sudo blocks are idempotent privileged hooks run by zordon (which
	// holds the user's terminal, so the password prompt works) after the
	// service comes up. `check` runs WITHOUT sudo — if it exits 0 the step
	// is already satisfied and `apply` is skipped (no password prompt).
	// `apply` runs as `sudo /bin/sh -c <apply>` only when check fails.
	// `verify` (optional) runs WITHOUT sudo after apply. All snippets are
	// evaluated after command, so they can interpolate self.* / parent refs.
	Sudo      []*sudoBlock `hcl:"sudo,block"`
	Files     []*fileBlock `hcl:"file,block"`
	Readiness *probeSpec   `hcl:"readiness,block"`
}

type sudoBlock struct {
	Name   string         `hcl:"name,label"`
	Check  hcl.Expression `hcl:"check,optional"`
	Apply  hcl.Expression `hcl:"apply"`
	Verify hcl.Expression `hcl:"verify,optional"`
}

type logBlock struct {
	Format string `hcl:"format,optional"`
	Filter string `hcl:"filter,optional"`
}

type probeSpec struct {
	HTTP             *httpActionSpec `hcl:"http,block"`
	InitialDelay     string          `hcl:"initial_delay,optional"`
	Period           string          `hcl:"period,optional"`
	Timeout          string          `hcl:"timeout,optional"`
	FailureThreshold int             `hcl:"failure_threshold,optional"`
	SuccessThreshold int             `hcl:"success_threshold,optional"`
}

type httpActionSpec struct {
	Path   string         `hcl:"path,optional"`
	Port   hcl.Expression `hcl:"port"`
	Host   string         `hcl:"host,optional"`
	Scheme string         `hcl:"scheme,optional"`
}

type fileBlock struct {
	Name string         `hcl:"name,label"`
	Path hcl.Expression `hcl:"path"`
	Body hcl.Expression `hcl:"body"`
}

// --- public entry point ---------------------------------------------------

// Open parses and resolves an Alphasfile. stateDir is what tmpdir() returns
// inside expressions (typically a deterministic per-Alphasfile directory
// computed by the caller via control.StateDir). Pass an empty stateDir to
// disable tmpdir() (it will error if used). parent pre-seeds the service
// namespace with values resolved by Alphasfiles higher in a federation
// chain; pass nil for a standalone Alphasfile.
func Open(path, stateDir string, parent *ParentContext) (*Alphasfile, error) {
	b, err := os.ReadFile(path)
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
	var seed map[string]map[string]cty.Value
	if parent != nil {
		seed = parent.byTC
	}
	return resolve(&root, stateDir, seed)
}

// ParentContext carries the resolved services of every Alphasfile above the
// current one in a federation chain, projected into cty so child expressions
// can reference them via the flat `service.<toolchain>.<name>` namespace.
type ParentContext struct {
	byTC map[string]map[string]cty.Value
}

// NewParentContext builds a ParentContext from already-resolved services
// (typically obtained from a running parent alpha via the state RPC).
// Re-projects each service into the same cty shape the resolver exposes:
// { name, toolchain, dir, vars, arguments, file }.
func NewParentContext(services []*Service) *ParentContext {
	pc := &ParentContext{byTC: map[string]map[string]cty.Value{}}
	for _, s := range services {
		if s == nil || s.Runtime == nil {
			continue
		}
		tc, name := s.Toolchain, s.Name()
		obj := map[string]cty.Value{
			"name":      cty.StringVal(name),
			"toolchain": cty.StringVal(tc),
			"dir":       cty.StringVal(serviceDirOf(s)),
			"vars":      mapValToCty(s.Runtime.Vars),
			"arguments": mapValToCty(s.Runtime.Arguments),
		}
		files := map[string]cty.Value{}
		for _, f := range s.Runtime.Files {
			files[f.Name] = cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal(f.Name),
				"path": cty.StringVal(f.Path),
				"body": cty.StringVal(f.Body),
			})
		}
		if len(files) > 0 {
			obj["file"] = cty.ObjectVal(files)
		} else {
			obj["file"] = cty.EmptyObjectVal
		}
		if pc.byTC[tc] == nil {
			pc.byTC[tc] = map[string]cty.Value{}
		}
		pc.byTC[tc][name] = cty.ObjectVal(obj)
	}
	return pc
}

// Merge folds another resolved Alphasfile's services into this context so a
// chain can accumulate top-down. Later levels see everything above them.
func (pc *ParentContext) Merge(services []*Service) *ParentContext {
	next := NewParentContext(services)
	for tc, byName := range next.byTC {
		if pc.byTC[tc] == nil {
			pc.byTC[tc] = map[string]cty.Value{}
		}
		for name, v := range byName {
			pc.byTC[tc][name] = v
		}
	}
	return pc
}

// --- value coercion -------------------------------------------------------

func ctyToAny(v cty.Value) any {
	if v.IsNull() {
		return nil
	}
	t := v.Type()
	switch {
	case t == cty.String:
		return v.AsString()
	case t == cty.Bool:
		return v.True()
	case t == cty.Number:
		bf := v.AsBigFloat()
		if i, acc := bf.Int64(); acc == big.Exact {
			return i
		}
		f, _ := bf.Float64()
		return f
	}
	return v.GoString()
}

// anyToCty converts a Go scalar to a cty.Value for the EvalContext. Used to
// expose already-evaluated arguments back into HCL space so later blocks can
// reference them via service.<tc>.<name>.arguments["..."].
func anyToCty(v any) cty.Value {
	switch x := v.(type) {
	case nil:
		return cty.NullVal(cty.DynamicPseudoType)
	case string:
		return cty.StringVal(x)
	case bool:
		return cty.BoolVal(x)
	case int:
		return cty.NumberIntVal(int64(x))
	case int64:
		return cty.NumberIntVal(x)
	case float64:
		return cty.NumberFloatVal(x)
	}
	return cty.StringVal(fmt.Sprintf("%v", v))
}

func compileProbe(ps *probeSpec, port int) (*probe.Probe, error) {
	p := &probe.Probe{
		FailureThreshold: ps.FailureThreshold,
		SuccessThreshold: ps.SuccessThreshold,
	}
	if ps.HTTP != nil {
		p.HTTP = &probe.HTTPAction{
			Path:   ps.HTTP.Path,
			Port:   port,
			Host:   ps.HTTP.Host,
			Scheme: ps.HTTP.Scheme,
		}
	}
	parse := func(field, raw string) (time.Duration, error) {
		if raw == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("%s=%q: %w", field, raw, err)
		}
		return d, nil
	}
	var err error
	if p.InitialDelay, err = parse("initial_delay", ps.InitialDelay); err != nil {
		return nil, err
	}
	if p.Period, err = parse("period", ps.Period); err != nil {
		return nil, err
	}
	if p.Timeout, err = parse("timeout", ps.Timeout); err != nil {
		return nil, err
	}
	return p, nil
}

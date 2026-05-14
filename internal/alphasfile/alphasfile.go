// Package alphasfile parses an Alphasfile (HCL2) into a serializable set of
// service definitions consumed by zordon and alpha.
//
// The runtime side (process spawn, install, etc.) lives elsewhere; here we
// only own the wire-stable data model.
//
// Schema (HCL2):
//
//	service "<toolchain>" "<name>" {
//	  // common runtime fields
//	  color           = "..."
//	  doubleDash      = true
//	  space_separated = true
//	  dir             = "..."
//	  arguments = {
//	    "key" = "value"
//	    ...
//	  }
//	  log {
//	    format = "json" | "plain"
//	    filter = "<regex>"
//	  }
//	  readiness {
//	    http { path = ..., port = ..., host = ..., scheme = ... }
//	    initial_delay     = "0s"
//	    period            = "200ms"
//	    timeout           = "1s"
//	    failure_threshold = 30
//	    success_threshold = 1
//	  }
//
//	  // toolchain-specific (see Service{Go,Rust,Ruby} below)
//	}
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

// LogConfig and below are the wire-stable JSON shape. They carry no hcl
// struct tags because parsing goes through a separate intermediate.
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
	Arguments      map[string]any `json:"arguments,omitempty"`
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

// Service is the flat, serializable view: package manifest + runtime config.
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

// NeedsSource reports whether the service has a source tree we should
// materialize (clone). Go always needs source (the import path is a repo).
// Rust/Ruby only when an explicit Git is provided.
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

// Source returns the canonical source string (Go import path or git URL).
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

// Branch returns the ref to check out: explicit Branch wins, Tag/Rev fall
// back when set (in that order). Empty string = default branch.
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

// Install returns the user-provided install command string.
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

type Alphasfile struct {
	Services []*Service `json:"services,omitempty"`
}

func (af *Alphasfile) All() []*Service { return af.Services }

// --- HCL2 intermediate (parser-internal) -----------------------------------

type rootBlock struct {
	Services []*serviceBlock `hcl:"service,block"`
}

type serviceBlock struct {
	Toolchain string `hcl:"toolchain,label"`
	Name      string `hcl:"name,label"`

	Color          string         `hcl:"color,optional"`
	Log            *logBlock      `hcl:"log,block"`
	DoubleDash     bool           `hcl:"doubleDash,optional"`
	SpaceSeparated bool           `hcl:"space_separated,optional"`
	Dir            string         `hcl:"dir,optional"`
	Arguments      hcl.Expression `hcl:"arguments,optional"`
	Readiness      *probeSpec     `hcl:"readiness,block"`

	// go
	Import string `hcl:"import,optional"`

	// shared go/rust/ruby
	Branch  string `hcl:"branch,optional"`
	Install string `hcl:"install,optional"`

	// rust
	Crate       string   `hcl:"crate,optional"`
	Version     string   `hcl:"version,optional"`
	Git         string   `hcl:"git,optional"`
	Tag         string   `hcl:"tag,optional"`
	Rev         string   `hcl:"rev,optional"`
	Bin         string   `hcl:"bin,optional"`
	Features    []string `hcl:"features,optional"`
	AllFeatures bool     `hcl:"all_features,optional"`
	Locked      bool     `hcl:"locked,optional"`

	// ruby
	Run string `hcl:"run,optional"`
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
	Path   string `hcl:"path,optional"`
	Port   int    `hcl:"port"`
	Host   string `hcl:"host,optional"`
	Scheme string `hcl:"scheme,optional"`
}

// Open parses an Alphasfile at path.
func Open(path string) (*Alphasfile, error) {
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

	af := &Alphasfile{}
	for _, sb := range root.Services {
		svc, err := convertService(sb)
		if err != nil {
			return nil, fmt.Errorf("alphasfile: service %q %q: %w", sb.Toolchain, sb.Name, err)
		}
		af.Services = append(af.Services, svc)
	}
	return af, nil
}

func convertService(sb *serviceBlock) (*Service, error) {
	args, err := evalArguments(sb.Arguments)
	if err != nil {
		return nil, err
	}

	rt := &RuntimeConfig{
		Name:           sb.Name,
		Color:          sb.Color,
		DoubleDash:     sb.DoubleDash,
		SpaceSeparated: sb.SpaceSeparated,
		Dir:            sb.Dir,
		Arguments:      args,
	}
	if sb.Log != nil {
		rt.Log = &LogConfig{Format: sb.Log.Format, Filter: sb.Log.Filter}
	}
	if sb.Readiness != nil {
		p, err := compileProbe(sb.Readiness)
		if err != nil {
			return nil, fmt.Errorf("readiness: %w", err)
		}
		rt.Readiness = p
	}

	svc := &Service{Toolchain: sb.Toolchain, Runtime: rt}
	switch sb.Toolchain {
	case ToolchainGo:
		svc.Go = &ServiceGo{Name: sb.Name, Import: sb.Import, Branch: sb.Branch, Install: sb.Install}
	case ToolchainRust:
		svc.Rust = &ServiceRust{
			Name: sb.Name, Crate: sb.Crate, Version: sb.Version, Git: sb.Git,
			Branch: sb.Branch, Tag: sb.Tag, Rev: sb.Rev, Bin: sb.Bin,
			Features: sb.Features, AllFeatures: sb.AllFeatures, Locked: sb.Locked,
			Install: sb.Install,
		}
	case ToolchainRuby:
		svc.Ruby = &ServiceRuby{
			Name: sb.Name, Git: sb.Git, Branch: sb.Branch, Tag: sb.Tag,
			Rev: sb.Rev, Install: sb.Install, Run: sb.Run,
		}
	default:
		return nil, fmt.Errorf("unknown toolchain %q (want go|rust|ruby)", sb.Toolchain)
	}
	return svc, nil
}

// evalArguments evaluates the `arguments` expression into a flat map. With a
// nil EvalContext only literal values resolve; dynamic helpers and cross-
// service references are introduced in a later phase.
func evalArguments(expr hcl.Expression) (map[string]any, error) {
	if expr == nil {
		return nil, nil
	}
	val, diags := expr.Value(nil)
	if diags.HasErrors() {
		return nil, fmt.Errorf("arguments: %s", diags.Error())
	}
	if val.IsNull() {
		return nil, nil
	}
	t := val.Type()
	if !t.IsObjectType() && !t.IsMapType() {
		return nil, fmt.Errorf("arguments must be a map/object, got %s", t.FriendlyName())
	}
	out := map[string]any{}
	for k, v := range val.AsValueMap() {
		out[k] = ctyToAny(v)
	}
	return out, nil
}

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

func compileProbe(ps *probeSpec) (*probe.Probe, error) {
	p := &probe.Probe{
		FailureThreshold: ps.FailureThreshold,
		SuccessThreshold: ps.SuccessThreshold,
	}
	if ps.HTTP != nil {
		p.HTTP = &probe.HTTPAction{
			Path:   ps.HTTP.Path,
			Port:   ps.HTTP.Port,
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

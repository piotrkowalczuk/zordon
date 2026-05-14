// Package alphasfile parses an Alphasfile (HCL) into a serializable set of
// service definitions consumed by zordon and alpha.
//
// The runtime side (process spawn, install, etc.) lives elsewhere; here we
// only own the wire-stable data model.
package alphasfile

import (
	"fmt"
	"os"
	"time"

	"github.com/hashicorp/hcl"

	"github.com/piotrkowalczuk/zordon/internal/probe"
)

const (
	ToolchainGo   = "go"
	ToolchainRust = "rust"
	ToolchainRuby = "ruby"
)

type LogConfig struct {
	Format string `hcl:"format" json:"format,omitempty"`
	Filter string `hcl:"filter" json:"filter,omitempty"`
}

type RuntimeConfig struct {
	Name           string         `hcl:"name,key" json:"name"`
	Color          string         `hcl:"color" json:"color,omitempty"`
	Log            *LogConfig     `hcl:"log,expand" json:"log,omitempty"`
	DoubleDash     bool           `hcl:"doubleDash" json:"double_dash,omitempty"`
	SpaceSeparated bool           `hcl:"space_separated" json:"space_separated,omitempty"`
	Arguments      map[string]any `hcl:"arguments" json:"arguments,omitempty"`
	Dir            string         `hcl:"dir" json:"dir,omitempty"`

	// HCL surface; converted to Readiness during Open.
	ReadinessSpec *probeSpec `hcl:"readiness,expand" json:"-"`

	// Resolved probe; what alpha consumes. Wire-stable via probe.Probe.
	Readiness *probe.Probe `hcl:"-" json:"readiness,omitempty"`
}

// probeSpec mirrors probe.Probe but with Go-duration strings (HCL-friendly).
type probeSpec struct {
	HTTP             *httpActionSpec `hcl:"http,expand"`
	InitialDelay     string          `hcl:"initial_delay"`
	Period           string          `hcl:"period"`
	Timeout          string          `hcl:"timeout"`
	FailureThreshold int             `hcl:"failure_threshold"`
	SuccessThreshold int             `hcl:"success_threshold"`
}

type httpActionSpec struct {
	Path   string `hcl:"path"`
	Port   int    `hcl:"port"`
	Host   string `hcl:"host"`
	Scheme string `hcl:"scheme"`
}

func (ps *probeSpec) compile() (*probe.Probe, error) {
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

type ServiceGo struct {
	Name    string `hcl:"name,key" json:"name"`
	Import  string `hcl:"import" json:"import"`
	Branch  string `hcl:"branch" json:"branch,omitempty"`
	Install string `hcl:"install" json:"install,omitempty"`
}

type ServiceRust struct {
	Name        string   `hcl:"name,key" json:"name"`
	Crate       string   `hcl:"crate" json:"crate,omitempty"`
	Version     string   `hcl:"version" json:"version,omitempty"`
	Git         string   `hcl:"git" json:"git,omitempty"`
	Branch      string   `hcl:"branch" json:"branch,omitempty"`
	Tag         string   `hcl:"tag" json:"tag,omitempty"`
	Rev         string   `hcl:"rev" json:"rev,omitempty"`
	Bin         string   `hcl:"bin" json:"bin,omitempty"`
	Features    []string `hcl:"features" json:"features,omitempty"`
	AllFeatures bool     `hcl:"all_features" json:"all_features,omitempty"`
	Locked      bool     `hcl:"locked" json:"locked,omitempty"`
	Install     string   `hcl:"install" json:"install,omitempty"`
}

type ServiceRuby struct {
	Name    string `hcl:"name,key" json:"name"`
	Git     string `hcl:"git" json:"git,omitempty"`
	Branch  string `hcl:"branch" json:"branch,omitempty"`
	Tag     string `hcl:"tag" json:"tag,omitempty"`
	Rev     string `hcl:"rev" json:"rev,omitempty"`
	Install string `hcl:"install" json:"install,omitempty"`
	Run     string `hcl:"run" json:"run,omitempty"`
}

// Service is a flat, serializable view: package manifest + runtime config.
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
// Rust/Ruby only when an explicit Git is provided — otherwise the binary
// comes from crates.io or a system gem.
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

// Install returns the user-provided install command string (e.g. "bundle
// install --path vendor/bundle"), if any. Empty means "no per-service
// install command beyond the toolchain default".
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
	Go   []*ServiceGo   `json:"go,omitempty"`
	Rust []*ServiceRust `json:"rust,omitempty"`
	Ruby []*ServiceRuby `json:"ruby,omitempty"`

	runtimeByID map[string]*RuntimeConfig
}

func (af *Alphasfile) All() []*Service {
	out := make([]*Service, 0, len(af.Go)+len(af.Rust)+len(af.Ruby))
	for _, g := range af.Go {
		out = append(out, &Service{
			Toolchain: ToolchainGo,
			Runtime:   af.runtime(ToolchainGo, g.Name),
			Go:        g,
		})
	}
	for _, r := range af.Rust {
		out = append(out, &Service{
			Toolchain: ToolchainRust,
			Runtime:   af.runtime(ToolchainRust, r.Name),
			Rust:      r,
		})
	}
	for _, r := range af.Ruby {
		out = append(out, &Service{
			Toolchain: ToolchainRuby,
			Runtime:   af.runtime(ToolchainRuby, r.Name),
			Ruby:      r,
		})
	}
	return out
}

func (af *Alphasfile) runtime(toolchain, name string) *RuntimeConfig {
	if rc, ok := af.runtimeByID[toolchain+"/"+name]; ok {
		return rc
	}
	return &RuntimeConfig{Name: name}
}

// Open parses an Alphasfile at path.
func Open(path string) (*Alphasfile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("alphasfile read: %w", err)
	}

	var pkg struct {
		Go   []*ServiceGo   `hcl:"service.go,expand"`
		Rust []*ServiceRust `hcl:"service.rust,expand"`
		Ruby []*ServiceRuby `hcl:"service.ruby,expand"`
	}
	if err := hcl.Unmarshal(b, &pkg); err != nil {
		return nil, fmt.Errorf("alphasfile package parse: %w", err)
	}

	var rt struct {
		Go   []*RuntimeConfig `hcl:"service.go,expand"`
		Rust []*RuntimeConfig `hcl:"service.rust,expand"`
		Ruby []*RuntimeConfig `hcl:"service.ruby,expand"`
	}
	if err := hcl.Unmarshal(b, &rt); err != nil {
		return nil, fmt.Errorf("alphasfile runtime parse: %w", err)
	}

	af := &Alphasfile{
		Go:          pkg.Go,
		Rust:        pkg.Rust,
		Ruby:        pkg.Ruby,
		runtimeByID: make(map[string]*RuntimeConfig, len(rt.Go)+len(rt.Rust)+len(rt.Ruby)),
	}
	for _, rc := range rt.Go {
		af.runtimeByID[ToolchainGo+"/"+rc.Name] = rc
	}
	for _, rc := range rt.Rust {
		af.runtimeByID[ToolchainRust+"/"+rc.Name] = rc
	}
	for _, rc := range rt.Ruby {
		af.runtimeByID[ToolchainRuby+"/"+rc.Name] = rc
	}

	for id, rc := range af.runtimeByID {
		if rc.ReadinessSpec == nil {
			continue
		}
		p, err := rc.ReadinessSpec.compile()
		if err != nil {
			return nil, fmt.Errorf("alphasfile: %s: readiness: %w", id, err)
		}
		rc.Readiness = p
	}

	return af, nil
}

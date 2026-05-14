package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/hashicorp/hcl"
)

const (
	ToolchainGo   = "go"
	ToolchainRust = "rust"
)

type variable struct {
	Default     any
	Description string
	Fields      []string `hcl:",decodedFields"`
}

// RuntimeConfig holds fields used while a service is running. Shared by all
// toolchains; decoded in a separate HCL pass and merged by Name.
type RuntimeConfig struct {
	Name       string         `hcl:"name,key"`
	Color      string         `hcl:"color"`
	Log        string         `hcl:"log"`
	DoubleDash bool           `hcl:"doubleDash"`
	Arguments  map[string]any `hcl:"arguments"`
	Dir        string         `hcl:"dir"`
}

// ServiceGo is the Go-toolchain package/install manifest. Field names mirror
// Go conventions: `import` is the canonical import path.
type ServiceGo struct {
	Name    string   `hcl:"name,key"`
	Import  string   `hcl:"import"`
	Branch  string   `hcl:"branch"`
	Install string   `hcl:"install"`
	Fields  []string `hcl:",decodedFields"`
}

// ServiceRust is the Rust-toolchain package/install manifest. Field names
// mirror `cargo install` flags and Cargo.toml dependency syntax.
type ServiceRust struct {
	Name        string   `hcl:"name,key"`
	Crate       string   `hcl:"crate"`        // crates.io crate name (defaults to Name)
	Version     string   `hcl:"version"`      // --version
	Git         string   `hcl:"git"`          // git source — when set, clone+local install
	Branch      string   `hcl:"branch"`
	Tag         string   `hcl:"tag"`
	Rev         string   `hcl:"rev"`
	Bin         string   `hcl:"bin"`          // --bin
	Features    []string `hcl:"features"`
	AllFeatures bool     `hcl:"all_features"` // --all-features
	Locked      bool     `hcl:"locked"`       // --locked
	Install     string   `hcl:"install"`
	Fields      []string `hcl:",decodedFields"`
}

type Alphasfile struct {
	Go          []*ServiceGo
	Rust        []*ServiceRust
	runtimeByID map[string]*RuntimeConfig // key: toolchain + "/" + name
	all         []*Service
}

// Service is the unified runtime view assembled from a package manifest plus
// the corresponding RuntimeConfig.
type Service struct {
	*RuntimeConfig
	Toolchain string
	pkg       packageManifest
}

func (s *Service) Flags() []string {
	out := make([]string, 0, len(s.Arguments))
	for flag, value := range s.Arguments {
		if s.DoubleDash {
			out = append(out, fmt.Sprintf("--%s=%v", flag, value))
			continue
		}
		out = append(out, fmt.Sprintf("-%s=%v", flag, value))
	}
	return out
}

func (s *Service) NeedsSource() bool                   { return s.pkg.needsSource() }
func (s *Service) Source() string                      { return s.pkg.source() }
func (s *Service) Branch() string                      { return s.pkg.branch() }
func (s *Service) CustomInstall() string               { return s.pkg.customInstall() }
func (s *Service) InstallCmd(src sourceInfo) *exec.Cmd { return s.pkg.installCmd(src, s.Name) }

type packageManifest interface {
	needsSource() bool
	source() string
	branch() string
	customInstall() string
	installCmd(src sourceInfo, serviceName string) *exec.Cmd
}

type goPkg struct{ s *ServiceGo }

func (g goPkg) needsSource() bool     { return true }
func (g goPkg) source() string        { return g.s.Import }
func (g goPkg) branch() string        { return g.s.Branch }
func (g goPkg) customInstall() string { return g.s.Install }
func (g goPkg) installCmd(src sourceInfo, _ string) *exec.Cmd {
	cmd := exec.Command("go", "install", src.subpath)
	cmd.Dir = src.repoDir
	return cmd
}

type rustPkg struct{ s *ServiceRust }

func (r rustPkg) needsSource() bool     { return r.s.Git != "" }
func (r rustPkg) source() string        { return r.s.Git }
func (r rustPkg) customInstall() string { return r.s.Install }
func (r rustPkg) branch() string {
	if r.s.Tag != "" {
		return r.s.Tag
	}
	if r.s.Rev != "" {
		return r.s.Rev
	}
	return r.s.Branch
}
func (r rustPkg) installCmd(src sourceInfo, serviceName string) *exec.Cmd {
	args := []string{"install"}
	if r.s.Locked {
		args = append(args, "--locked")
	}
	if r.s.AllFeatures {
		args = append(args, "--all-features")
	} else if len(r.s.Features) > 0 {
		args = append(args, "--features", strings.Join(r.s.Features, ","))
	}
	if r.s.Git != "" {
		bin := r.s.Bin
		if bin == "" {
			bin = serviceName
		}
		args = append(args, "--bin", bin, "--path", ".")
		cmd := exec.Command("cargo", args...)
		cmd.Dir = src.repoDir
		return cmd
	}
	if r.s.Version != "" {
		args = append(args, "--version", r.s.Version)
	}
	crate := r.s.Crate
	if crate == "" {
		crate = serviceName
	}
	args = append(args, crate)
	return exec.Command("cargo", args...)
}

// All returns the merged runtime view of every service.
func (af *Alphasfile) All() []*Service {
	if af.all != nil {
		return af.all
	}
	out := make([]*Service, 0, len(af.Go)+len(af.Rust))
	for _, g := range af.Go {
		out = append(out, &Service{
			RuntimeConfig: af.runtime(ToolchainGo, g.Name),
			Toolchain:     ToolchainGo,
			pkg:           goPkg{s: g},
		})
	}
	for _, r := range af.Rust {
		out = append(out, &Service{
			RuntimeConfig: af.runtime(ToolchainRust, r.Name),
			Toolchain:     ToolchainRust,
			pkg:           rustPkg{s: r},
		})
	}
	af.all = out
	return out
}

func (af *Alphasfile) runtime(toolchain, name string) *RuntimeConfig {
	if rc, ok := af.runtimeByID[toolchain+"/"+name]; ok {
		return rc
	}
	return &RuntimeConfig{Name: name}
}

func openAlphasfile(path string) (*Alphasfile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("alpha: Alphasfile read error: %s", err.Error())
	}

	// Pass 1: package/install manifests (toolchain-specific fields).
	var pkg struct {
		Variables map[string]*variable `hcl:"variable,"`
		Go        []*ServiceGo         `hcl:"service.go,expand"`
		Rust      []*ServiceRust       `hcl:"service.rust,expand"`
	}
	if err = hcl.Unmarshal(b, &pkg); err != nil {
		return nil, fmt.Errorf("alpha: Alphasfile package parse error: %s", err.Error())
	}

	// Pass 2: shared runtime fields (same HCL keys, different target struct).
	var rt struct {
		Go   []*RuntimeConfig `hcl:"service.go,expand"`
		Rust []*RuntimeConfig `hcl:"service.rust,expand"`
	}
	if err = hcl.Unmarshal(b, &rt); err != nil {
		return nil, fmt.Errorf("alpha: Alphasfile runtime parse error: %s", err.Error())
	}

	af := &Alphasfile{
		Go:          pkg.Go,
		Rust:        pkg.Rust,
		runtimeByID: make(map[string]*RuntimeConfig, len(rt.Go)+len(rt.Rust)),
	}
	for _, rc := range rt.Go {
		af.runtimeByID[ToolchainGo+"/"+rc.Name] = rc
	}
	for _, rc := range rt.Rust {
		af.runtimeByID[ToolchainRust+"/"+rc.Name] = rc
	}

	for i, s := range af.All() {
		if s.Color == "" {
			s.Color = servicePalette[i%len(servicePalette)]
		}
	}

	return af, nil
}

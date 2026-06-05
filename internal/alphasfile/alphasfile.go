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
	"maps"
	"math/big"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/probe"
	"github.com/piotrkowalczuk/zordon/internal/zenv"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

const (
	ToolchainGo   = "go"
	ToolchainRust = "rust"
	ToolchainRuby = "ruby"
	// ToolchainNode is "nodejs" — same label for the service block and
	// the toolchain sub-block (1:1). The mise tool name is "node", which
	// the install path uses directly; this constant is the zordon-side
	// identifier (block label + service label).
	ToolchainNode = "nodejs"
)

// --- wire-stable types (JSON over the control socket) ---------------------

type LogConfig struct {
	Format string `json:"format,omitempty"`
	Filter string `json:"filter,omitempty"`
	// TTY overrides the toolchain-derived "give the child a PTY?" choice.
	// nil = use default for the toolchain (currently only ruby = true).
	// Use a PTY when the runtime block-buffers stdout under a pipe (Ruby)
	// and you want live log lines to flow without source-code changes.
	// The cost is one PTY per service plus a few µs/line for line discipline.
	TTY *bool `json:"tty,omitempty"`
}

type RuntimeConfig struct {
	Name           string                    `json:"name"`
	Color          string                    `json:"color,omitempty"`
	Log            *LogConfig                `json:"log,omitempty"`
	DoubleDash     bool                      `json:"double_dash,omitempty"`
	SpaceSeparated bool                      `json:"space_separated,omitempty"`
	Vars           map[string]any            `json:"vars,omitempty"`
	Arguments      map[string]any            `json:"arguments,omitempty"`
	Env            zenv.EnvironmentVariables `json:"env,omitempty"` // explicit env (overrides dotenv)
	// Phase-scoped env. BuildEnv is injected only while zordon builds the
	// service (go build / cargo install / bundle); RunEnv only into the
	// running process; AgentEnv is an override layer applied on top (for
	// build and run) when alpha was configured with --agent — lets an AI
	// caller e.g. turn down log verbosity without touching the Alphasfile.
	BuildEnv zenv.EnvironmentVariables `json:"build_env,omitempty"`
	RunEnv   zenv.EnvironmentVariables `json:"run_env,omitempty"`
	AgentEnv zenv.EnvironmentVariables `json:"agent_env,omitempty"`
	Dotenv   []string                  `json:"dotenv,omitempty"` // paths to .env files loaded before Env (in order)
	Command  []string                  `json:"command,omitempty"`
	// After holds runtime-level barrier refs. alpha's per-service
	// bringup goroutine selects on each before starting cmd; with no
	// entries the service starts in parallel with everything else.
	// Same canonical-string grammar as ProvisionStep.After.
	After     []string         `json:"after,omitempty"`
	Sudo      []*SudoStep      `json:"sudo,omitempty"`
	Provision []*ProvisionStep `json:"provision,omitempty"`
	Files     []*File          `json:"files,omitempty"`
	Dir       string           `json:"dir,omitempty"`      // service working dir = <checkout>/<exe> (= fs::exe / self.dir)
	Checkout  string           `json:"checkout,omitempty"` // checkout root (= fs::src); the `git worktree add` target
	BinDir    string           `json:"bin_dir,omitempty"`  // per-invocation build output (= fs::bin)
	Print     string           `json:"print,omitempty"`    // extra `zordon status` line (resolved)
	Readiness *probe.Probe     `json:"readiness,omitempty"`
}

// Package is the unified, toolchain-tagged source/build manifest. A
// service's primary is exactly one of: Git (zordon-owned bare clone or
// user-owned local checkout via Src), or Install (rust crate from a
// registry / go module). Build overrides the per-toolchain default.
type Package struct {
	Toolchain string `json:"toolchain"` // go|rust|ruby
	Git       string `json:"git,omitempty"`
	Src       string `json:"src,omitempty"` // Relates to the local checkout path
	Branch    string `json:"branch,omitempty"`
	Tag       string `json:"tag,omitempty"`
	Rev       string `json:"rev,omitempty"`
	// Use-only: install a dependency instead of giving it a worktree.
	// Install is the resolved coordinate — Go: `pkg@version` for
	// `go install`; Rust: the crate name for `cargo install`. Features is
	// Rust-only. Mutually exclusive with git/src.
	Install  string    `json:"install,omitempty"`
	Version  string    `json:"version,omitempty"`  // rust use-only: cargo install --version
	Index    string    `json:"index,omitempty"`    // rust use-only: cargo install --index
	Registry string    `json:"registry,omitempty"` // rust use-only: cargo install --registry
	Features []string  `json:"features,omitempty"`
	Exe      string    `json:"exe,omitempty"`       // relative subdir within git/src where the build target lives ("" = project root)
	Bin      string    `json:"bin,omitempty"`       // rust: cargo --bin target (multi-bin crates)
	BuildCmd []string  `json:"build_cmd,omitempty"` // explicit build argv (from build{cmd}); empty ⇒ toolchain default
	Cmd      string    `json:"cmd,omitempty"`       // Explicit execution argv if needed
	Worktree *Worktree `json:"worktree,omitempty"`
	// InPlace: src-only in the "main" worktree — build/run from src as-is,
	// no git worktree add, no HEAD reset (the edit→start loop).
	InPlace bool `json:"in_place,omitempty"`
}

type Worktree struct {
	Sparse []string `json:"sparse,omitempty"`
}

type Service struct {
	Toolchain string          `json:"toolchain"`
	Runtime   *RuntimeConfig  `json:"runtime"`
	Package   *Package        `json:"package,omitempty"`
	Debugger  *DebuggerConfig `json:"debugger,omitempty"`
	Agent     *ServiceAgent   `json:"agent,omitempty"`
}

type ServiceAgent struct {
	// Keyed by feature name so `service.X.agent.mcp.<feature>.<field>`
	// lookups don't need positional indexing.
	MCP map[string]*AgentMCPFeature `json:"mcp,omitempty"`
}

type DebuggerConfig struct {
	Enabled bool `json:"enabled"`
	// Port: 0 from this struct means "picked by the eval pass" — alpha
	// always sees a concrete number.
	Port          int  `json:"port"`
	WaitForClient bool `json:"wait_for_client,omitempty"`
	Log           bool `json:"log,omitempty"`
	MCP           bool `json:"mcp,omitempty"`
	WrapRuntime   bool `json:"wrap_runtime,omitempty"`
}

type AgentMCPFeature struct {
	Name    string `json:"name"`
	Bridge  string `json:"bridge"`
	Address string `json:"address,omitempty"`
}

// ServiceMeta is the static, eval-free view of a service: identity plus its
// source primary. `zordon worktree` uses it to materialize checkouts
// without resolving the dynamic graph (no Invocation / parent context /
// pickport needed just to `git worktree add`).
type ServiceMeta struct {
	Name      string
	Toolchain string
	Package   *Package
}

// Ref is the default revision for this service's worktree (Branch, then
// Tag, then Rev; empty = primary HEAD).
func (m *ServiceMeta) Ref() string {
	switch {
	case m.Package == nil:
		return ""
	case m.Package.Branch != "":
		return m.Package.Branch
	case m.Package.Tag != "":
		return m.Package.Tag
	case m.Package.Rev != "":
		return m.Package.Rev
	}
	return ""
}

// Worktreeable reports whether this service has a git/src primary.
func (m *ServiceMeta) Worktreeable() bool {
	return m.Package != nil && (m.Package.Git != "" || m.Package.Src != "")
}

// ParseServices does a parse-only decode of an Alphasfile: it returns each
// service's identity and source primary without evaluating any expression
// (vars / arguments / files / readiness / sudo are ignored). Pure, needs no
// Invocation — the entry point for `zordon worktree`.
func ParseServices(path string) ([]*ServiceMeta, error) {
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
	base := filepath.Dir(path) // relative src/sparse anchor = Alphasfile dir
	out := make([]*ServiceMeta, 0, len(root.Services))
	for _, sb := range root.Services {
		pkg := &Package{Toolchain: sb.Toolchain}
		if sb.Crate != nil && (sb.Git != nil || (sb.Src != nil && sb.Src.Path != "")) {
			return nil, fmt.Errorf("service %q: crate {} cannot coexist with src{path}/git", sb.Name)
		}
		if sb.Git != nil && sb.Src != nil && sb.Src.Path != "" {
			return nil, fmt.Errorf("service %q: src{path} and git{} are mutually exclusive (src can only carry exe alongside git)", sb.Name)
		}
		if sb.Git != nil {
			pkg.Git = sb.Git.URL
			pkg.Branch = sb.Git.Branch
			pkg.Tag = sb.Git.Tag
			pkg.Rev = sb.Git.Rev
		}
		if sb.Src != nil {
			pkg.Src = resolveSrcDir(base, sb.Src.Path)
			pkg.Exe = sb.Src.Exe
		}
		if sb.Crate != nil {
			pkg.Install = strings.TrimSpace(sb.Crate.Name)
		}
		if sb.Worktree != nil && len(sb.Worktree.Sparse) > 0 {
			pkg.Worktree = &Worktree{Sparse: cleanSparse(sb.Worktree.Sparse)}
		}
		out = append(out, &ServiceMeta{Name: sb.Name, Toolchain: sb.Toolchain, Package: pkg})
	}
	return out, nil
}

// resolveSrcDir turns a `src` value into an absolute path: ~ expands to
// $HOME; absolute stays; relative resolves against base (the Alphasfile's
// directory). Empty stays empty.
func resolveSrcDir(base, dir string) string {
	if dir == "" {
		return ""
	}
	if strings.HasPrefix(dir, "~/") {
		if home, err := zenv.UserHomeDir(); err == nil {
			return filepath.Join(home, dir[2:])
		}
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Clean(filepath.Join(base, dir))
}

// cleanSparse normalizes sparse-checkout entries to repo-root-relative cone
// paths (git sparse-checkout wants paths relative to the repo top, not
// absolute and without a leading "./").
func cleanSparse(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = filepath.Clean(strings.TrimPrefix(s, "./"))
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

func (s *Service) Name() string {
	if s.Runtime != nil {
		return s.Runtime.Name
	}
	return ""
}

// Ref returns the default revision for git worktree add: Branch wins, then
// Tag, then Rev. Empty = primary HEAD.
func (s *Service) Ref() string {
	if s.Package == nil {
		return ""
	}
	switch {
	case s.Package.Branch != "":
		return s.Package.Branch
	case s.Package.Tag != "":
		return s.Package.Tag
	case s.Package.Rev != "":
		return s.Package.Rev
	}
	return ""
}

// Worktreeable reports whether the service has a git/src primary (so it can
// get a per-invocation checkout). Crate services don't (cargo install).
func (s *Service) Worktreeable() bool {
	return s.Package != nil && !s.UseOnly() &&
		(s.Package.Git != "" || s.Package.Src != "")
}

// UseOnly reports a use-only service: installed (go/cargo install) into
// fs::bin, no worktree, not editable.
func (s *Service) UseOnly() bool {
	return s.Package != nil && s.Package.Install != ""
}

// Buildable reports whether zordon produces this service's binary itself
// (a checkout build, or a use-only install). Every service must be
// buildable — there is no "use a prebuilt $PATH binary" path.
func (s *Service) Buildable() bool {
	return s.Worktreeable() || s.UseOnly()
}

// BuildCmd returns the explicit build argv (from `build { cmd = [...] }`),
// or nil for the toolchain default.
func (s *Service) BuildCmd() []string {
	if s.Package == nil {
		return nil
	}
	return s.Package.BuildCmd
}

// toolchainDefaults holds the per-toolchain answers to "what should this
// field be when the user didn't say?". One place to look when adding a new
// toolchain; one place to look when answering "why did my service get X?".
//
// Resolved during eval, baked into the wire-stable *Service so alpha never
// needs to know about toolchain identities — it just reads the resolved
// fields. Add new defaults here as they appear (env nudges for python,
// readiness defaults, etc.).
type toolchainDefaults struct {
	// TTY: hand the child a PTY for stdout so it line-buffers naturally.
	//   - go:     false (os.Stdout writes through to write(2))
	//   - rust:   false (println! uses LineWriter, flushes on '\n')
	//   - ruby:   true  (STDOUT.sync defaults to false on pipes — startup
	//                    logs would stay buffered until process exit)
	//   - nodejs: true  (process.stdout switches to block-buffered when
	//                    piped; same startup-log-buffering pitfall as Ruby)
	TTY bool
}

var toolchainDefaultsFor = map[string]toolchainDefaults{
	ToolchainGo:   {TTY: false},
	ToolchainRust: {TTY: false},
	ToolchainRuby: {TTY: true},
	ToolchainNode: {TTY: true},
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

// ProvisionStep is one declared bringup-time action attached to a
// service's runtime. Same shape as sudo (check/cmd/verify) plus a
// per-step env overlay and an `after` dependency list. Provisions run
// during bringup after the parent service becomes ready (implicit dep),
// so bringup isn't reported complete until they've all finished.
//
// after entries name what this step depends on; alpha resolves them at
// runtime against the live service set:
//
//	"<name>"                                local provision in the same service
//	"service.<tc>.<svc>.ready"              wait for another service ready
//	"service.<tc>.<svc>.provision.<name>"   wait for another provision
type ProvisionStep struct {
	Name   string                    `json:"name"`
	Check  string                    `json:"check,omitempty"`
	Cmd    string                    `json:"cmd"`
	Verify string                    `json:"verify,omitempty"`
	Env    zenv.EnvironmentVariables `json:"env,omitempty"`
	// After holds the resolved barrier refs ("service.go.db@ready",
	// "service.go.api.runtime.provision.create-tables@success", ...).
	// alpha parses these at bringup and selects on (target, terminal-
	// failure) pairs to keep waiters deadlock-free.
	After []string `json:"after,omitempty"`
	// Detached: when true, bringup completion (EventDone) doesn't wait
	// for this provision to finish. Useful for warmups / notifications
	// that shouldn't block declaring the stack "up". Detached failures
	// don't trigger failfast (you opted out of synchronous coupling).
	Detached bool `json:"detached,omitempty"`
	// Latent: provision declared with `after = never`. It is registered
	// (its barrier resolves, and it can be used as a CmdRef template) but
	// NOT auto-run at bringup. A latent provision is a reusable action a
	// service exposes for *other* services to invoke for themselves —
	// e.g. a kafka service declares `create-topic` once; each consumer
	// invokes it with its own TOPIC. Mutually exclusive with After refs.
	Latent bool `json:"latent,omitempty"`
	// CmdRef, when set, means this provision has no inline cmd of its
	// own: it runs another (latent) provision's resolved check/cmd/
	// verify snippets, but under THIS provision's barrier and with THIS
	// step's env overlay (layered on the invoking service's env). Value
	// is the canonical target ID
	// "service.<tc>.<svc>.runtime.provision.<name>". The snippet is the
	// provider's (resolved at the provider's eval time); parameterization
	// flows through env the snippet reads at runtime (e.g. ${TOPIC}).
	// Note: the snippet runs in the *invoking* service's cwd.
	CmdRef string `json:"cmd_ref,omitempty"`
}

type Alphasfile struct {
	// CfgHash is the manifest identity (sha8 of Alphasfile bytes + resolved
	// parent ctx) — the value cfg::hash() returns. Carried on the resolved
	// result, not on the Invocation (which is pure directory facts).
	CfgHash  string     `json:"cfg_hash,omitempty"`
	Dotenv   []string   `json:"dotenv,omitempty"` // file-level .env files, applied under every service's env (in order)
	Services []*Service `json:"services,omitempty"`
	// Toolchain pins the version+env of each language toolchain
	// alpha materializes via mise. Keyed by the same string as
	// service.Toolchain (`go`, `rust`, `ruby`). Federation children
	// inherit from parents; the resolver merges down the chain with
	// child overriding on collision. Empty/absent = use whatever
	// `go`/`cargo`/`bundle` is on PATH (status quo, no mise).
	Toolchain map[string]*ToolchainConfig `json:"toolchain,omitempty"`
	// SysEnv is the closed-world whitelist of OS env vars that pass
	// through from alpha's parent shell into every spawned service /
	// build / provision. Default-deny: anything not in this list is
	// stripped. Federation merges parent ∪ child (no widening that
	// would silently expose more of the host than each level chose).
	//
	// Why default-deny: an interactive shell with a version manager
	// activated exports per-language path vars (RUBYLIB / GEM_HOME /
	// BUNDLE_* / PYTHONPATH / CARGO_HOME / ...) pointing at the user's
	// tool world. Those override mise's PATH ordering, so a service
	// "pinned" via toolchain{} silently loads its tooling from the
	// shell's install — or mixes interpreters from incompatible
	// installs. Whitelisting forces the user to declare exactly what
	// the spawn is allowed to inherit.
	SysEnv []string `json:"sysenv,omitempty"`
}

// ToolchainConfig declares the version a particular language toolchain
// should be pinned to, the tools to install into that pinned version
// (each version has its OWN tool world — a tool installed for one
// patch is not visible to another), and an env overlay applied to
// every service of this toolchain (and its provisions).
type ToolchainConfig struct {
	Version string `json:"version"`
	// Tools is a name→version map of language-native tools alpha
	// ensures are installed under the pinned interpreter before any
	// service starts. "Tool" rather than "package" because these are
	// auxiliary binaries used by the toolchain (bundler, dlv, cargo-
	// watch), not library deps of the user's code. The install command
	// is language-specific:
	//   ruby   → `gem install <name> --version <ver> --no-document`
	//   python → `pip install <name>==<ver>`
	//   rust   → `cargo install <name> --version <ver> --locked`
	//   go     → `go install <name>@<ver>`
	// All run through `mise exec <lang>@<version> -- ...` so they
	// land in the right version's tool world.
	Tools map[string]string         `json:"tools,omitempty"`
	Env   zenv.EnvironmentVariables `json:"env,omitempty"`
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
	Dotenv    hcl.Expression  `hcl:"dotenv,optional"` // file-level .env applied to every service
	SysEnv    hcl.Expression  `hcl:"sysenv,optional"` // closed-world whitelist of inherited OS env vars
	Services  []*serviceBlock `hcl:"service,block"`
	Toolchain *toolchainBlock `hcl:"toolchain,block"`
}

// toolchainBlock is the optional top-level pin map. Each language gets
// its own named sub-block — keeping them typed (rather than a generic
// labeled block) means `toolchain { go { ... } nodejs { ... } }`
// catches typos at decode time, not at runtime in alpha.
type toolchainBlock struct {
	Go     *langToolchainBlock `hcl:"go,block"`
	Rust   *langToolchainBlock `hcl:"rust,block"`
	Ruby   *langToolchainBlock `hcl:"ruby,block"`
	Nodejs *langToolchainBlock `hcl:"nodejs,block"`
}

type langToolchainBlock struct {
	Version string         `hcl:"version"`
	Tools   hcl.Expression `hcl:"tools,optional"`
	Env     hcl.Expression `hcl:"env,optional"`
}

// byLabel projects the typed sub-blocks back into a label-keyed map so
// the resolver can iterate without caring about which languages exist.
func (t *toolchainBlock) byLabel() map[string]*langToolchainBlock {
	out := map[string]*langToolchainBlock{}
	if t.Go != nil {
		out[ToolchainGo] = t.Go
	}
	if t.Rust != nil {
		out[ToolchainRust] = t.Rust
	}
	if t.Ruby != nil {
		out[ToolchainRuby] = t.Ruby
	}
	if t.Nodejs != nil {
		out[ToolchainNode] = t.Nodejs
	}
	return out
}

type serviceBlock struct {
	Toolchain string `hcl:"toolchain,label"`
	Name      string `hcl:"name,label"`

	// static fields
	Color          string    `hcl:"color,optional"`
	Log            *logBlock `hcl:"log,block"`
	DoubleDash     bool      `hcl:"doubleDash,optional"`
	SpaceSeparated bool      `hcl:"space_separated,optional"`

	// Source: exactly one of `src { ... }` (local checkout, no clone),
	// `git { ... }` (remote — zordon clones), or `crate { ... }` (rust
	// use-only install via `cargo install`). Go services also pick:
	// src/git block XOR top-level `package` (go install <pkg@ver>).
	// `bin` / `features` are rust-only artifact knobs and apply to all
	// source modes.
	Src      *srcBlock   `hcl:"src,block"`
	Git      *gitBlock   `hcl:"git,block"`
	Crate    *crateBlock `hcl:"crate,block"`
	Package  string      `hcl:"package,optional"`  // go use-only: `go install <package>` (incl. @version)
	Bin      string      `hcl:"bin,optional"`      // rust: cargo --bin target
	Features []string    `hcl:"features,optional"` // rust: cargo --features (applies to crate-install AND worktree build)

	// dynamic (interpolated; order resolved by intra-service dependency DAG)
	Vars      hcl.Expression `hcl:"vars,optional"`
	Arguments hcl.Expression `hcl:"arguments,optional"`
	Env       hcl.Expression `hcl:"env,optional"`
	Dotenv    hcl.Expression `hcl:"dotenv,optional"`
	// Print is an extra line shown per service in `zordon status` —
	// interpolated, so e.g. a Go service can surface its live endpoint:
	// print = "http://127.0.0.1:${self.vars.port}/". URLs are emitted as
	// clickable terminal hyperlinks.
	Print hcl.Expression `hcl:"print,optional"`

	// Phase blocks — each a full phase, not just an env container:
	// `cmd` (argv list) + `env` (interpolated, joins the DAG). `build`
	// is the build step (empty ⇒ toolchain default); `runtime` is the
	// run step (its `cmd` is the service argv — there is no top-level
	// `cmd`); `agent` is an env overlay applied in `--agent` mode (it
	// has no `cmd`).
	Build    *buildBlock    `hcl:"build,block"`
	Runtime  *runtimeBlock  `hcl:"runtime,block"`
	Agent    *agentBlock    `hcl:"agent,block"`
	Debugger *debuggerBlock `hcl:"debugger,block"`

	Worktree  *worktreeBlock `hcl:"worktree,block"`
	Sudo      []*sudoBlock   `hcl:"sudo,block"`
	Files     []*fileBlock   `hcl:"file,block"`
	Readiness *probeSpec     `hcl:"readiness,block"`
}

// build/runtime/agent are distinct phases with distinct payloads (not
// one generic block): `cmd` is an argv list — no implicit shell, use
// ["sh","-lc","..."] when you need one — and `env` is interpolated,
// joining the intra-service DAG like any other field.

// buildBlock is the build step: `cmd` is the build command (empty ⇒
// toolchain default); `env` is injected only while building
// (go build / cargo install / bundle), never into the running process.
type buildBlock struct {
	Cmd hcl.Expression `hcl:"cmd,optional"`
	Env hcl.Expression `hcl:"env,optional"`
}

// runtimeBlock is the run step: `cmd` is the service argv (there is no
// top-level `cmd`); `env` is the running process env. `after` is a list
// of barrier refs (same grammar as provision.after) — the service's
// own bringup goroutine selects on each before starting cmd, so
// runtime-level ordering is *explicit*: with no `after`, services come
// up in parallel and may race each other.
//
// `provision` blocks are bringup-time actions that fire once their own
// `after` deps are satisfied.
type runtimeBlock struct {
	Cmd       hcl.Expression    `hcl:"cmd,optional"`
	Env       hcl.Expression    `hcl:"env,optional"`
	After     hcl.Expression    `hcl:"after,optional"`
	Provision []*provisionBlock `hcl:"provision,block"`
}

// provisionBlock is one bringup action. cmd is required; check/verify
// are interpolated shell snippets like sudo's. env overlays the
// service's runtime env for this step only. after is an HCL list whose
// elements are barrier traversals (e.g. service.go.db.ready); keeping
// it as hcl.Expression lets the DAG resolver see those traversals and
// order service evaluation accordingly. detached=true means bringup
// doesn't wait for this provision's success before sending EventDone.
type provisionBlock struct {
	Name     string         `hcl:"name,label"`
	Check    hcl.Expression `hcl:"check,optional"`
	Cmd      hcl.Expression `hcl:"cmd"`
	Verify   hcl.Expression `hcl:"verify,optional"`
	Env      hcl.Expression `hcl:"env,optional"`
	After    hcl.Expression `hcl:"after,optional"`
	Detached bool           `hcl:"detached,optional"`
}

// agentBlock overlays `env` on top of build AND runtime, but only when
// alpha is configured with `zordon --agent` (an AI/automation caller).
// It has no `cmd` — it never starts anything, only adjusts env.
type agentBlock struct {
	Env hcl.Expression `hcl:"env,optional"`
}

type debuggerBlock struct {
	Enabled       bool           `hcl:"enabled,optional"`
	Port          hcl.Expression `hcl:"port,optional"`
	WaitForClient *bool          `hcl:"wait_for_client,optional"`
	Log           *bool          `hcl:"log,optional"`
	MCP           *bool          `hcl:"mcp,optional"`
	WrapRuntime   *bool          `hcl:"wrap_runtime,optional"`
}

type worktreeBlock struct {
	Sparse []string `hcl:"sparse,optional"`
}

// gitBlock is a remote-git source. zordon bare-clones `url` and
// `branch`/`tag`/`rev` pin the revision. The build-target subdir
// (`exe`) is described by a sibling `src { }` block.
type gitBlock struct {
	URL    string `hcl:"url"`
	Branch string `hcl:"branch,optional"`
	Tag    string `hcl:"tag,optional"`
	Rev    string `hcl:"rev,optional"`
}

// srcBlock describes the local view of the source. Standalone (no
// sibling `git { }`) it IS the source — `path` is the local checkout
// zordon uses in place. Alongside `git { }` it carries only `exe`,
// the subdir within the cloned worktree to build. `path` is therefore
// optional, but at least one of `path`/`exe` must be set.
type srcBlock struct {
	Path string `hcl:"path,optional"`
	Exe  string `hcl:"exe,optional"`
}

// crateBlock is the rust use-only primary: `cargo install <name>`.
// Mirrors the cargo install CLI source-selection flags: `version`/
// `index`/`registry` for a registry source, or `git`+`branch`/`tag`/
// `rev` for a git source (cargo handles the fetch — no zordon
// worktree). Presence of this block is mutually exclusive with the
// top-level `git {}` block (worktree/source path). `features` and
// `bin` are top-level on the service.
type crateBlock struct {
	Name     string `hcl:"name"`
	Version  string `hcl:"version,optional"`
	Index    string `hcl:"index,optional"`
	Registry string `hcl:"registry,optional"`
	Git      string `hcl:"git,optional"`
	Branch   string `hcl:"branch,optional"`
	Tag      string `hcl:"tag,optional"`
	Rev      string `hcl:"rev,optional"`
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
	TTY    *bool  `hcl:"tty,optional"`
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

// Open reads, parses and resolves an Alphasfile at path. inv carries the
// invocation identity (hash, tmp dir, per-service checkout paths); parent
// pre-seeds the flat service namespace with values resolved by Alphasfiles
// higher in a federation chain (nil for a standalone file).
func Open(path string, inv *invocation.InvocationState, parent *ParentContext, cfgHash string, testCfg TestConfig) (*Alphasfile, error) {
	b, err := zfs.Read(path)
	if err != nil {
		return nil, fmt.Errorf("alphasfile read: %w", err)
	}
	return Compile(path, b, inv, parent, cfgHash, testCfg)
}

// Compile is Open without filesystem I/O: it resolves the given Alphasfile
// source bytes. Pure (no process spawn, no clone) — the unit-test entry
// point for the whole resolution pipeline. name is only used in diagnostics.
//
// testCfg gates the test:: HCL functions; the zero value (production
// default) leaves them erroring out so a non-harness Alphasfile that
// uses test::log() fails fast.
func Compile(name string, src []byte, inv *invocation.InvocationState, parent *ParentContext, cfgHash string, testCfg TestConfig) (*Alphasfile, error) {
	m, err := NewManifestState(name, src, inv)
	if err != nil {
		return nil, err
	}
	p, err := m.Plan(parent, cfgHash, testCfg)
	if err != nil {
		return nil, err
	}
	return p.Compute()
}

// ManifestState is a parsed-but-not-evaluated Alphasfile placed in its
// invocation: the raw block tree ready to be planned and computed. Parent-free
// — federation parent context enters at Plan, not here.
type ManifestState struct {
	name string
	root *rootBlock
	inv  *invocation.InvocationState
}

// NewManifestState parses Alphasfile source into its block tree. This is the
// only step that fails on HCL syntax; ordering (Plan) and evaluation (Compute)
// come after.
func NewManifestState(name string, src []byte, inv *invocation.InvocationState) (*ManifestState, error) {
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(src, name)
	if diags.HasErrors() {
		return nil, fmt.Errorf("alphasfile parse: %s", diags.Error())
	}
	var root rootBlock
	if diags := gohcl.DecodeBody(file.Body, nil, &root); diags.HasErrors() {
		return nil, fmt.Errorf("alphasfile decode: %s", diags.Error())
	}
	return &ManifestState{name: name, root: &root, inv: inv}, nil
}

// ParentContext carries what a federation child needs from its parents:
// resolved services projected into cty (so cross-service refs work) AND
// the cumulative toolchain pins (so the child inherits go/rust/ruby
// versions from above without having to re-declare).
type ParentContext struct {
	byTC      map[string]map[string]cty.Value
	toolchain map[string]*ToolchainConfig
	sysenv    []string
}

// NewParentContext builds a ParentContext from already-resolved services
// (typically obtained from a running parent alpha via the state RPC).
// Re-projects each service into the same cty shape the resolver exposes:
// { name, toolchain, dir, vars, arguments, file }.
func NewParentContext(services []*Service) *ParentContext {
	pc := &ParentContext{
		byTC:      map[string]map[string]cty.Value{},
		toolchain: map[string]*ToolchainConfig{},
	}
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
		maps.Copy(pc.byTC[tc], byName)
	}
	return pc
}

// WithToolchain returns a context with the given toolchain map layered
// over what's already there. Later calls (children, deeper in the
// chain) override earlier entries on collision — exactly the federation
// "child wins" semantic the rest of the system uses.
func (pc *ParentContext) WithToolchain(tc map[string]*ToolchainConfig) *ParentContext {
	if pc.toolchain == nil {
		pc.toolchain = map[string]*ToolchainConfig{}
	}
	maps.Copy(pc.toolchain, tc)
	return pc
}

// Toolchain returns the accumulated toolchain pins so the resolver can
// merge them into the child's own (child wins).
func (pc *ParentContext) Toolchain() map[string]*ToolchainConfig {
	return pc.toolchain
}

// WithSysEnv returns a context with the given sysenv names unioned with
// what's already there. Order-preserving with stable dedup so the merge
// is deterministic across runs (federation chains hash the resulting
// Alphasfile and any list reorder would flap cfg::hash()).
func (pc *ParentContext) WithSysEnv(names []string) *ParentContext {
	pc.sysenv = mergeSysEnv(pc.sysenv, names)
	return pc
}

// SysEnv returns the accumulated whitelist from the federation chain.
func (pc *ParentContext) SysEnv() []string {
	return pc.sysenv
}

// BlockComputedState is the computed state of one federation level: the static
// facts it contributes to its children (services / toolchain / sysenv / dotenv)
// plus its manifest identity (CfgHash). Both a freshly resolved Alphasfile
// (Alphasfile.Block) and an adopted running alpha (AdoptBlock) produce one, so
// GlobalComputedState.Join folds a STATE — not a loose glue struct, and there
// is a single result type regardless of source.
type BlockComputedState struct {
	services  []*Service
	toolchain map[string]*ToolchainConfig
	sysenv    []string
	dotenv    []string
	cfgHash   string
}

func (b BlockComputedState) Services() []*Service                   { return b.services }
func (b BlockComputedState) Toolchain() map[string]*ToolchainConfig { return b.toolchain }
func (b BlockComputedState) SysEnv() []string                       { return b.sysenv }
func (b BlockComputedState) Dotenv() []string                       { return b.dotenv }
func (b BlockComputedState) CfgHash() string                        { return b.cfgHash }

// Block is the fresh-eval constructor: the resolved Alphasfile's own computed
// state of this level.
func (af *Alphasfile) Block() BlockComputedState {
	return BlockComputedState{
		services:  af.Services,
		toolchain: af.Toolchain,
		sysenv:    af.SysEnv,
		dotenv:    af.Dotenv,
		cfgHash:   af.CfgHash,
	}
}

// AdoptBlock is the reuse constructor: the computed state of a running alpha,
// rebuilt from its reported facts (the caller reads these off a
// protocol.StateInfo, which this package can't import without a cycle).
func AdoptBlock(services []*Service, toolchain map[string]*ToolchainConfig, sysenv, dotenv []string, cfgHash string) BlockComputedState {
	return BlockComputedState{
		services:  services,
		toolchain: toolchain,
		sysenv:    sysenv,
		dotenv:    dotenv,
		cfgHash:   cfgHash,
	}
}

// GlobalComputedState accumulates the resolved facts of every federation
// ancestor, root-first, into the single parent context a child level evaluates
// against. The zero value is the empty context the chain root sees — usable
// without construction. Join mutates the receiver in place (math/big style);
// pass it down the chain as a single accumulator, not by value copy.
type GlobalComputedState struct {
	services  []*Service
	toolchain map[string]*ToolchainConfig
	sysenv    []string
	dotenv    []string
}

// Join folds one level's computed block into the accumulator, preserving the
// chain's merge rules: services append (the resolver de-dups on seed via
// NewParentContext, child overriding parent by name), toolchain map-merges
// with the later (deeper/child) entry winning, sysenv unions order-preserving,
// dotenv appends root-first. CfgHash is per-level identity, not accumulated.
func (g *GlobalComputedState) Join(b BlockComputedState) {
	g.services = append(g.services, b.services...)
	if len(b.toolchain) > 0 {
		if g.toolchain == nil {
			g.toolchain = make(map[string]*ToolchainConfig, len(b.toolchain))
		}
		maps.Copy(g.toolchain, b.toolchain)
	}
	g.sysenv = mergeSysEnv(g.sysenv, b.sysenv)
	g.dotenv = append(g.dotenv, b.dotenv...)
}

// Services returns the accumulated services (root-first, undeduped) — the slice
// federation marshals into the parent-context bytes feeding cfg::hash().
func (g *GlobalComputedState) Services() []*Service { return g.services }

// Dotenv returns the accumulated file-level dotenv paths, root-first — the
// parentDotenv a child alpha layers under its own services' env.
func (g *GlobalComputedState) Dotenv() []string { return g.dotenv }

// ParentContext projects the accumulator into the *ParentContext the resolver
// consumes, or nil when nothing has accumulated yet (the root level passes nil,
// exactly as before). Dotenv is excluded by design — it flows via Dotenv(), not
// through eval.
func (g *GlobalComputedState) ParentContext() *ParentContext {
	if len(g.services) == 0 && len(g.toolchain) == 0 && len(g.sysenv) == 0 {
		return nil
	}
	return NewParentContext(g.services).WithToolchain(g.toolchain).WithSysEnv(g.sysenv)
}

// mergeSysEnv unions two sysenv lists preserving the order of first
// occurrence (a, then any new keys from b). Empty entries dropped.
func mergeSysEnv(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string(nil), a...), b...) {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
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

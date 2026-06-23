// Package protocol defines the JSON-lines wire format between zordon (client)
// and alpha (server).
//
// Each message is a single JSON object terminated by '\n'. The client writes
// a Request and reads a Response per round-trip; connections are short-lived
// by convention but the protocol itself is connection-agnostic.
package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

type Op string

const (
	OpConfigure Op = "configure"
	OpState     Op = "state"
	OpShutdown  Op = "shutdown"
	OpInvoke    Op = "invoke"
	// OpClean runs each provision's `clean` teardown snippet against a
	// stopped stack. It reuses Request.Configure as its payload (the
	// resolved Alphasfile + paths): alpha materializes toolchains and
	// rebuilds service env without spawning the services, then runs the
	// clean snippets in reverse order. Streams the same Event sequence as
	// Configure and never triggers failfast shutdown.
	OpClean Op = "clean"
)

type Request struct {
	Op        Op             `json:"op"`
	Configure *ConfigureArgs `json:"configure,omitempty"`
	Invoke    *InvokeArgs    `json:"invoke,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// InvokeArgs runs a single provision on demand inside the live alpha. The
// provision's snippets, parent service env, cwd and toolchain are reused as-is;
// Env is overlaid on top (highest precedence) so a caller can parametrize a
// latent template (e.g. TOPIC for kafka's create-topic). The run streams the
// same Event sequence as Configure and never triggers failfast shutdown — a
// failed invoke reports failure but leaves alpha running.
type InvokeArgs struct {
	// Provision is the canonical id: service.<tc>.<svc>.runtime.provision.<name>.
	Provision string `json:"provision"`
	// Args are the provision's declared, typed arguments (placeholder
	// substitution); Env is the free-form environment overlay. Both reach the
	// shell, but Args are validated against the provision's `argument` blocks.
	Args map[string]any    `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
}

type ConfigureArgs struct {
	AlphasfilePath string                 `json:"alphasfile_path"`
	Alphasfile     *alphasfile.Alphasfile `json:"alphasfile"`
	Failfast       bool                   `json:"failfast,omitempty"`
	// Agent mirrors `zordon --agent`: when set, each service's
	// `agent { env {} }` overlay is applied on top of build and run env.
	Agent bool `json:"agent,omitempty"`
	// FsHash is the filesystem-location identity of this invocation (depends
	// only on the directory). Sent for symmetry / verification; the socket
	// path already encodes it.
	FsHash string `json:"fs_hash,omitempty"`
	// CfgHash is the manifest identity: sha of (Alphasfile bytes + resolved
	// parent context). zordon compares it against a running alpha's stored
	// CfgHash to detect drift in federation chains.
	CfgHash string `json:"cfg_hash,omitempty"`
	// ParentDotenv carries file-level `dotenv` paths from every Alphasfile
	// higher in the federation chain (root-first). Each service applies
	// these (then its own file-level dotenv, then its per-service env).
	ParentDotenv []string `json:"parent_dotenv,omitempty"`
	// ParentEnv carries file-level inline `env` accumulated from every
	// Alphasfile higher in the federation chain (deeper ancestor winning on
	// collision). Layered under this level's own file-level env, which is in
	// turn under each service's own dotenv/env.
	ParentEnv map[string]string `json:"parent_env,omitempty"`
}

type Response struct {
	OK    bool       `json:"ok"`
	Error string     `json:"error,omitempty"`
	State *StateInfo `json:"state,omitempty"`
}

// Event is a single message in a streamed response (currently only Configure).
// The stream terminates with kind = EventDone or kind = EventError.
type Event struct {
	Kind    string `json:"kind"`
	Service string `json:"service,omitempty"`
	Stream  string `json:"stream,omitempty"` // "stdout" or "stderr" for log lines
	Line    string `json:"line,omitempty"`
	Error   string `json:"error,omitempty"`
}

const (
	EventLog          = "log"
	EventServiceStart = "service_start"
	EventServiceReady = "service_ready"
	EventServiceFail  = "service_fail"
	EventDone         = "done"
	EventError        = "error"
)

type StateInfo struct {
	PID            int                                    `json:"pid"`
	AlphasfilePath string                                 `json:"alphasfile_path,omitempty"`
	StartedAt      string                                 `json:"started_at"`
	FsHash         string                                 `json:"fs_hash,omitempty"`  // instance identity (location)
	CfgHash        string                                 `json:"cfg_hash,omitempty"` // manifest identity (Alphasfile+parent ctx)
	Dotenv         []string                               `json:"dotenv,omitempty"`   // file-level dotenv (for federation chain)
	Env            map[string]string                      `json:"env,omitempty"`      // file-level inline env (for federation chain)
	Services       []*alphasfile.Service                  `json:"services,omitempty"`
	Toolchain      map[string]*alphasfile.ToolchainConfig `json:"toolchain,omitempty"` // for federation child inheritance
	SysEnv         []string                               `json:"sysenv,omitempty"`    // closed-world env whitelist (federation accumulates)
	Running        []ServiceStatus                        `json:"running,omitempty"`
}

type ServiceStatus struct {
	Name      string `json:"name"`
	PID       int    `json:"pid"`
	Readiness string `json:"readiness,omitempty"` // "probing", "ready", "failed"
}

// Encoder writes newline-delimited JSON messages.
type Encoder struct{ w io.Writer }

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

func (e *Encoder) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	b = append(b, '\n')
	if _, err := e.w.Write(b); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// Decoder reads newline-delimited JSON messages.
type Decoder struct{ s *bufio.Scanner }

func NewDecoder(r io.Reader) *Decoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 8*1024*1024)
	return &Decoder{s: s}
}

func (d *Decoder) Read(v any) error {
	if !d.s.Scan() {
		if err := d.s.Err(); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		return io.EOF
	}
	if err := json.Unmarshal(d.s.Bytes(), v); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}

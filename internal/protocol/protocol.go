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
)

type Request struct {
	Op        Op                     `json:"op"`
	Configure *ConfigureArgs         `json:"configure,omitempty"`
	Extra     map[string]any         `json:"extra,omitempty"`
}

type ConfigureArgs struct {
	AlphasfilePath string                 `json:"alphasfile_path"`
	Alphasfile     *alphasfile.Alphasfile `json:"alphasfile"`
	Failfast       bool                   `json:"failfast,omitempty"`
	// ConfigHash identifies the (source bytes + parent context) that produced
	// this configuration. zordon compares it against a running alpha's stored
	// hash to detect drift in federation chains.
	ConfigHash string `json:"config_hash,omitempty"`
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
	PID            int                   `json:"pid"`
	AlphasfilePath string                `json:"alphasfile_path,omitempty"`
	StartedAt      string                `json:"started_at"`
	ConfigHash     string                `json:"config_hash,omitempty"`
	Services       []*alphasfile.Service `json:"services,omitempty"`
	Running        []ServiceStatus       `json:"running,omitempty"`
}

type ServiceStatus struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
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

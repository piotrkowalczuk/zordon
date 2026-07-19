// Package zlog is the single logging primitive used by zordon and alpha.
//
// The output format switches on Agent:
//
//   - Agent=false (human):  "[<RFC3339>] <src> [<LEVEL>] - <msg>"
//   - Agent=true  (machine): "<ms-since-start> <src> <LEVEL> <msg>"
//
// LEVEL is INFO / ERROR for explicit logging or STDOUT / STDERR for forwarded
// service output (so log consumers can split streams in agent mode).
//
// In human mode the <src> column is colorized when the writer is a TTY, with
// "zordon" and "alpha" getting fixed accents and other sources hashed into a
// palette. Agent mode is always plain text.
package zlog

import (
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"
)

// Service palette: deterministic by hash. zordon/alpha are special-cased
// below so they stay visually distinct from any service.
var servicePalette = []string{
	"\x1b[36m", // cyan
	"\x1b[32m", // green
	"\x1b[33m", // yellow
	"\x1b[34m", // blue
	"\x1b[93m", // light yellow
	"\x1b[96m", // light cyan
	"\x1b[92m", // light green
}

type Logger struct {
	mu        sync.Mutex
	w         io.Writer
	agent     bool
	color     bool
	startedAt time.Time
}

// New writes to w. Format is dictated by agent; colors are applied in human
// mode only when w is a terminal.
func New(w io.Writer, agent bool) *Logger {
	return &Logger{
		w:         w,
		agent:     agent,
		color:     !agent && isTerminal(w),
		startedAt: time.Now(),
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func (l *Logger) Info(src, format string, args ...any) {
	l.emit(src, "INFO", fmt.Sprintf(format, args...))
}

func (l *Logger) Error(src, format string, args ...any) {
	l.emit(src, "ERROR", fmt.Sprintf(format, args...))
}

func (l *Logger) Warn(src, format string, args ...any) {
	l.emit(src, "WARN", fmt.Sprintf(format, args...))
}

// Service writes a raw line forwarded from a child's stdout/stderr.
// stream ("stdout"/"stderr") is preserved as the level in agent mode so
// consumers can split streams; folded to LOG in human mode.
func (l *Logger) Service(src, stream, line string) {
	level := "LOG"
	if l.agent && stream != "" {
		level = strings.ToUpper(stream)
	}
	l.emit(src, level, line)
}

func (l *Logger) emit(src, level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.agent {
		fmt.Fprintf(l.w, "%d %s %s %s\n",
			time.Since(l.startedAt).Milliseconds(), src, level, msg)
		return
	}
	srcCol := fmt.Sprintf("%-15s", src)
	tail := ""
	if l.color {
		srcCol = colorFor(src) + srcCol + ansiReset
		// Reset SGR at the end of every line. A forwarded service line can
		// leave a colour open — and dropping the line that carried the
		// matching reset (log filtering) makes that common — which would
		// otherwise bleed into the lines that follow.
		tail = ansiReset
	}
	fmt.Fprintf(l.w, "[%s] %s [%-5s] - %s%s\n",
		time.Now().Format(time.RFC3339), srcCol, level, msg, tail)
}

func colorFor(src string) string {
	switch src {
	case "zordon":
		return ansiBold
	case "alpha":
		return "\x1b[95m" // light magenta
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(src))
	return servicePalette[int(h.Sum32()%uint32(len(servicePalette)))] // #nosec G115 -- palette len is a small const, fits uint32
}

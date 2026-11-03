package control

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

func TestIsNotRunning(t *testing.T) {
	c1, c2 := net.Pipe()
	_ = c2.Close()
	var resp protocol.Response
	pipeReadErr := protocol.NewDecoder(c1).Read(&resp)
	_ = c1.Close()

	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":          {err: nil, want: false},
		"not exist":    {err: os.ErrNotExist, want: true},
		"eof":          {err: io.EOF, want: true},
		"pipe closed":  {err: pipeReadErr, want: true},
		"econnreset":   {err: syscall.ECONNRESET, want: true},
		"epipe":        {err: syscall.EPIPE, want: true},
		"dial opErr":   {err: &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}, want: true},
		"read timeout": {err: &net.OpError{Op: "read", Net: "unix", Err: os.ErrDeadlineExceeded}, want: false},
		"generic err":  {err: errors.New("boom"), want: false},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := IsNotRunning(c.err); got != c.want {
				t.Errorf("IsNotRunning(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

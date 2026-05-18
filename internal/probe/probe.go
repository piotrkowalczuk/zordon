// Package probe owns the readiness-probe runtime type and runner.
//
// Schema mirrors Kubernetes Probe semantics, but with Go-style duration values
// instead of *Seconds integers. HCL parsing of the duration strings lives in
// internal/alphasfile to keep this package free of HCL ties.
package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Probe struct {
	HTTP *HTTPAction `json:"http,omitempty"`

	InitialDelay     time.Duration `json:"initial_delay,omitempty"`
	Period           time.Duration `json:"period,omitempty"`
	Timeout          time.Duration `json:"timeout,omitempty"`
	FailureThreshold int           `json:"failure_threshold,omitempty"`
	SuccessThreshold int           `json:"success_threshold,omitempty"`
}

type HTTPAction struct {
	Path   string `json:"path,omitempty"`
	Port   int    `json:"port"`
	Host   string `json:"host,omitempty"`
	Scheme string `json:"scheme,omitempty"` // "http" (default) or "https"
}

// Defaults match k8s.
const (
	defaultPeriod           = 10 * time.Second
	defaultTimeout          = 1 * time.Second
	defaultFailureThreshold = 3
	defaultSuccessThreshold = 1
)

func (p *Probe) period() time.Duration {
	if p.Period <= 0 {
		return defaultPeriod
	}
	return p.Period
}

func (p *Probe) timeout() time.Duration {
	if p.Timeout <= 0 {
		return defaultTimeout
	}
	return p.Timeout
}

func (p *Probe) failureThreshold() int {
	if p.FailureThreshold <= 0 {
		return defaultFailureThreshold
	}
	return p.FailureThreshold
}

func (p *Probe) successThreshold() int {
	if p.SuccessThreshold <= 0 {
		return defaultSuccessThreshold
	}
	return p.SuccessThreshold
}

// Wait runs the probe until it succeeds successThreshold times in a row, or
// fails failureThreshold times in a row. Each attempt is reported via report
// (optional) so callers can log per-attempt status.
func (p *Probe) Wait(ctx context.Context, report func(ok bool, reason string)) error {
	if p == nil {
		return errors.New("nil probe")
	}
	if p.HTTP == nil {
		return errors.New("no probe action configured")
	}

	if p.InitialDelay > 0 {
		select {
		case <-time.After(p.InitialDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	var (
		fails int
		wins  int
	)
	failureMax := p.failureThreshold()
	successMax := p.successThreshold()
	for {
		ok, reason := p.try(ctx, p.timeout())
		if report != nil {
			report(ok, reason)
		}
		if ok {
			fails = 0
			wins++
			if wins >= successMax {
				return nil
			}
		} else {
			wins = 0
			fails++
			if fails >= failureMax {
				return fmt.Errorf("probe failed %d times in a row: %s", failureMax, reason)
			}
		}

		select {
		case <-time.After(p.period()):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Check performs a single probe attempt. Returns nil on success, or an error
// with the failure reason.
func (p *Probe) Check(ctx context.Context) error {
	if p == nil {
		return errors.New("nil probe")
	}
	ok, reason := p.try(ctx, p.timeout())
	if ok {
		return nil
	}
	return errors.New(reason)
}

func (p *Probe) try(ctx context.Context, timeout time.Duration) (bool, string) {
	if p.HTTP != nil {
		return p.tryHTTP(ctx, timeout)
	}
	return false, "no action"
}

func (p *Probe) tryHTTP(ctx context.Context, timeout time.Duration) (bool, string) {
	h := p.HTTP
	scheme := strings.ToLower(h.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	host := h.Host
	if host == "" {
		host = "127.0.0.1"
	}
	path := h.Path
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url := fmt.Sprintf("%s://%s:%d%s", scheme, host, h.Port, path)

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "build: " + err.Error()
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return true, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

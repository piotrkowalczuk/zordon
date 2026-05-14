package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestProbe_defaults(t *testing.T) {
	p := &Probe{}
	if p.period() != defaultPeriod {
		t.Errorf("expected default period, got %v", p.period())
	}
	if p.timeout() != defaultTimeout {
		t.Errorf("expected default timeout, got %v", p.timeout())
	}
	if p.failureThreshold() != defaultFailureThreshold {
		t.Errorf("expected default failure threshold, got %v", p.failureThreshold())
	}
	if p.successThreshold() != defaultSuccessThreshold {
		t.Errorf("expected default success threshold, got %v", p.successThreshold())
	}
}

func TestProbe_Wait_httpSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())

	p := &Probe{
		HTTP: &HTTPAction{
			Host: u.Hostname(),
			Port: port,
			Path: "/",
		},
		Period:           10 * time.Millisecond,
		Timeout:          100 * time.Millisecond,
		FailureThreshold: 1,
		SuccessThreshold: 2, // requires 2 consecutive successes
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	count := 0
	err := p.Wait(ctx, func(ok bool, reason string) {
		if ok {
			count++
		}
	})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 successful probe attempts, got %d", count)
	}
}

func TestProbe_Wait_httpFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())

	p := &Probe{
		HTTP: &HTTPAction{
			Host: u.Hostname(),
			Port: port,
		},
		Period:           10 * time.Millisecond,
		FailureThreshold: 3,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := p.Wait(ctx, nil)
	if err == nil {
		t.Fatal("expected error after failures, got nil")
	}
}

func TestProbe_Wait_httpTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())

	p := &Probe{
		HTTP: &HTTPAction{
			Host: u.Hostname(),
			Port: port,
		},
		Period:           10 * time.Millisecond,
		Timeout:          10 * time.Millisecond, // shorter than sleep
		FailureThreshold: 1,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := p.Wait(ctx, nil)
	if err == nil {
		t.Fatal("expected error due to timeout, got nil")
	}
}

func TestProbe_Wait_contextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())

	p := &Probe{
		HTTP: &HTTPAction{
			Host: u.Hostname(),
			Port: port,
		},
		Period:           1 * time.Hour, // Long period to prove synctest skips it
		FailureThreshold: 10,            // Lots of failures
	}

	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Wait(ctx, nil)
	}()

	// Wait briefly in bubble time, then cancel the context
	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error, got %v", err)
	}
}

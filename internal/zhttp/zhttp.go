// Package zhttp provides a bounded HTTP client wrapper with mandatory timeouts,
// redirect limits, and response-size caps.
package zhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Options configures a Client. Timeout is mandatory (> 0).
type Options struct {
	Timeout               time.Duration
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	MaxRedirects          int
}

// Client wraps http.Client with bounded timeouts and redirect policies.
type Client struct {
	hc *http.Client
}

// NewClient constructs a Client with an isolated transport and strict timeouts.
func NewClient(opts Options) *Client {
	if opts.Timeout <= 0 {
		panic("zhttp: NewClient requires Timeout > 0")
	}
	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 15 * time.Second
	}
	tlsTimeout := opts.TLSHandshakeTimeout
	if tlsTimeout <= 0 {
		tlsTimeout = 15 * time.Second
	}
	hdrTimeout := opts.ResponseHeaderTimeout
	if hdrTimeout <= 0 {
		hdrTimeout = 30 * time.Second
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = defaultMaxRedirects
	}

	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: hdrTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Client{
		hc: &http.Client{
			Transport:     transport,
			Timeout:       opts.Timeout,
			CheckRedirect: redirectPolicy(maxRedirects),
		},
	}
}

// Download fetches rawURL into dst, rejecting responses larger than maxBytes.
func (c *Client) Download(ctx context.Context, rawURL string, maxBytes int64, dst io.Writer) (int64, error) {
	if c == nil || c.hc == nil {
		panic("zhttp: Download called on nil Client")
	}
	if ctx == nil {
		panic("zhttp: Download requires non-nil context")
	}
	if dst == nil {
		panic("zhttp: Download requires non-nil destination writer")
	}
	if maxBytes <= 0 {
		panic("zhttp: Download requires maxBytes > 0")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, fmt.Errorf("zhttp: invalid url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return 0, fmt.Errorf("zhttp: unsupported url scheme %q", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, fmt.Errorf("zhttp: build request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("zhttp: get %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(snippet))
		if msg != "" {
			return 0, fmt.Errorf("zhttp: %s returned HTTP %d: %s", rawURL, resp.StatusCode, msg)
		}
		return 0, fmt.Errorf("zhttp: %s returned HTTP %d", rawURL, resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return 0, fmt.Errorf("zhttp: %s content-length %d exceeds limit %d", rawURL, resp.ContentLength, maxBytes)
	}

	n, err := io.Copy(dst, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return n, fmt.Errorf("zhttp: read %s: %w", rawURL, err)
	}
	if n > maxBytes {
		return n, fmt.Errorf("zhttp: %s body exceeds limit %d", rawURL, maxBytes)
	}
	return n, nil
}

const defaultMaxRedirects = 5

func redirectPolicy(maxRedirects int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("zhttp: stopped after %d redirects", maxRedirects)
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("zhttp: refusing redirect from https to non-https")
		}
		return nil
	}
}

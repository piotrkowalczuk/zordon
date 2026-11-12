package zhttp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClient_Download(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("payload-bytes"))
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/missing":
			http.Error(w, "not found", http.StatusNotFound)
		case "/huge":
			_, _ = w.Write([]byte(strings.Repeat("x", 128)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewClient(Options{Timeout: 5 * time.Second})

	cases := map[string]struct {
		path     string
		maxBytes int64
		want     string
		wantErr  string
	}{
		"direct ok": {
			path:     "/ok",
			maxBytes: 64,
			want:     "payload-bytes",
		},
		"follows redirect": {
			path:     "/redirect",
			maxBytes: 64,
			want:     "payload-bytes",
		},
		"http 404": {
			path:     "/missing",
			maxBytes: 64,
			wantErr:  "HTTP 404",
		},
		"exceeds maxBytes": {
			path:     "/huge",
			maxBytes: 32,
			wantErr:  "exceeds limit 32",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			var buf bytes.Buffer
			_, err := client.Download(t.Context(), srv.URL+c.path, c.maxBytes, &buf)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("Download(%s) err = %v, want substring %q", c.path, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Download(%s): %v", c.path, err)
			}
			if buf.String() != c.want {
				t.Errorf("Download(%s) = %q, want %q", c.path, buf.String(), c.want)
			}
		})
	}
}

func TestNewClient_zeroTimeoutPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewClient with Timeout=0 did not panic")
		}
	}()
	_ = NewClient(Options{})
}

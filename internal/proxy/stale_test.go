package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pandeylakshya207-max/servemesh/internal/backend"
	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

// The server closes the connection the first time it sees a request on a reused
// connection, simulating a backend closing an idle connection as a request arrives.
func TestStaleConnectionRetriedOnFreshConnectionWithoutEjection(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	var killed atomic.Int32
	inner := backend.NewMock(backend.MockConfig{ID: "m"}).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			mu.Lock()
			reused := seen[r.RemoteAddr]
			seen[r.RemoteAddr] = true
			mu.Unlock()
			if reused && killed.Add(1) == 1 {
				if hj, ok := w.(http.Hijacker); ok {
					if c, _, err := hj.Hijack(); err == nil {
						c.Close()
					}
				}
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	b := router.NewBackend("m", strings.TrimPrefix(srv.URL, "http://"))
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{b})

	if resp, _ := chat(t, gw, chatJSON("first")); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request status %d", resp.StatusCode)
	}
	resp, body := chat(t, gw, chatJSON("second"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("second request: status %d, body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Gateway-Attempts"); got != "1" {
		t.Fatalf("attempts = %s, want 1 (the stale-connection retry happens inside one attempt)", got)
	}
	if !b.Healthy() {
		t.Fatal("a stale pooled connection must not eject a healthy backend")
	}
	if got := gatewayStats(t, gw)["stale_conn_retries"]; got != float64(1) {
		t.Fatalf("stale_conn_retries = %v, want 1", got)
	}
}

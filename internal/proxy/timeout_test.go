package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

func TestSlowBackendIsNotEjectedOrRetried(t *testing.T) {
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slowSrv.Close)
	slow := router.NewBackend("slow", strings.TrimPrefix(slowSrv.URL, "http://"))
	good := startMock(t, "good")

	g := New(router.NewRoundRobin(), []*router.Backend{slow, good})
	g.SetResponseHeaderTimeout(150 * time.Millisecond)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	resp, _ := chat(t, srv, chatJSON("hello"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status %d, want 504", resp.StatusCode)
	}
	if !slow.Healthy() {
		t.Fatal("one slow response must not eject the backend")
	}
	if slow.InFlight() != 0 || good.InFlight() != 0 {
		t.Fatalf("in-flight leak: slow=%d good=%d", slow.InFlight(), good.InFlight())
	}
	if got := gatewayStats(t, srv)["upstream_timeouts"]; got != float64(1) {
		t.Fatalf("upstream_timeouts = %v, want 1", got)
	}
}

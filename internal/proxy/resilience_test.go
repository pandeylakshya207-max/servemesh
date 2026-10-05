package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/backend"
	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

// startFlaky starts a backend that streams two tokens and then either closes the
// connection abruptly (abort=true) or ends cleanly without [DONE] (abort=false).
func startFlaky(t *testing.T, id string, abort bool) *router.Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Backend-ID", id)
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 2; i++ {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok \"}}]}\n\n")
			w.(http.Flusher).Flush()
		}
		if abort {
			panic(http.ErrAbortHandler)
		}
	}))
	t.Cleanup(srv.Close)
	return router.NewBackend(id, strings.TrimPrefix(srv.URL, "http://"))
}

func startStatus(t *testing.T, id string, status int) *router.Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return router.NewBackend(id, strings.TrimPrefix(srv.URL, "http://"))
}

func gatewayStats(t *testing.T, gw *httptest.Server) map[string]any {
	t.Helper()
	resp, err := http.Get(gw.URL + "/backends")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRetryOnConnectFailure(t *testing.T) {
	dead := router.NewBackend("dead", "127.0.0.1:1")
	good := startMock(t, "good")
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{dead, good})

	resp, body := chat(t, gw, chatJSON("hello"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Backend-ID"); got != "good" {
		t.Fatalf("served by %q, want good", got)
	}
	if got := resp.Header.Get("X-Gateway-Attempts"); got != "2" {
		t.Fatalf("attempts %q, want 2", got)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatal("retried stream did not complete")
	}
	if dead.Healthy() {
		t.Fatal("unreachable backend was not ejected")
	}
	if dead.InFlight() != 0 || good.InFlight() != 0 {
		t.Fatalf("in-flight leak: dead=%d good=%d", dead.InFlight(), good.InFlight())
	}
}

func TestRetryOn5xx(t *testing.T) {
	bad := startStatus(t, "bad", http.StatusServiceUnavailable)
	good := startMock(t, "good")
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{bad, good})

	resp, _ := chat(t, gw, chatJSON("hello"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Backend-ID") != "good" {
		t.Fatalf("status %d via %q; want 200 via good", resp.StatusCode, resp.Header.Get("X-Backend-ID"))
	}
	if bad.Healthy() {
		t.Fatal("5xx backend was not ejected")
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	rejecting := startStatus(t, "r", http.StatusBadRequest)
	good := startMock(t, "good")
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{rejecting, good})

	resp, _ := chat(t, gw, chatJSON("hello"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want the backend's 400 passed through", resp.StatusCode)
	}
	if !rejecting.Healthy() {
		t.Fatal("a 4xx must not eject the backend")
	}
}

func TestAllBackendsFailReturns502(t *testing.T) {
	bs := []*router.Backend{
		router.NewBackend("d1", "127.0.0.1:1"),
		router.NewBackend("d2", "127.0.0.1:1"),
	}
	gw := startGateway(t, router.NewRoundRobin(), bs)
	resp, _ := chat(t, gw, chatJSON("hello"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}

func TestMidStreamAbortIsReportedNotRetried(t *testing.T) {
	flaky := startFlaky(t, "flaky", true)
	good := startMock(t, "good")
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{flaky, good})

	resp, body := chat(t, gw, chatJSON("hello"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 (stream had already started)", resp.StatusCode)
	}
	if resp.Header.Get("X-Backend-ID") != "flaky" || resp.Header.Get("X-Gateway-Attempts") != "1" {
		t.Fatalf("a started stream must not be retried: backend=%q attempts=%q",
			resp.Header.Get("X-Backend-ID"), resp.Header.Get("X-Gateway-Attempts"))
	}
	if !strings.Contains(body, "upstream_error") {
		t.Fatalf("no error event in truncated stream: %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatal("truncated stream must not look complete")
	}
	if flaky.Healthy() {
		t.Fatal("mid-stream failure did not eject the backend")
	}
	if got := gatewayStats(t, gw)["midstream_failures"]; got != float64(1) {
		t.Fatalf("midstream_failures = %v, want 1", got)
	}
}

func TestStreamEndingWithoutDoneIsFlagged(t *testing.T) {
	flaky := startFlaky(t, "flaky", false)
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{flaky})
	_, body := chat(t, gw, chatJSON("hello"))
	if !strings.Contains(body, "upstream_error") {
		t.Fatalf("stream without [DONE] was not flagged: %q", body)
	}
}

func TestClientDisconnectKeepsBackendHealthy(t *testing.T) {
	srv := httptest.NewServer(backend.NewMock(backend.MockConfig{
		ID: "slow", DecodePerToken: 50 * time.Millisecond,
	}).Handler())
	t.Cleanup(srv.Close)
	b := router.NewBackend("slow", strings.TrimPrefix(srv.URL, "http://"))
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{b})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	for b.InFlight() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b.InFlight() != 0 {
		t.Fatalf("in-flight = %d after client disconnect, want 0", b.InFlight())
	}
	if !b.Healthy() {
		t.Fatal("a client disconnect must not mark the backend unhealthy")
	}
}

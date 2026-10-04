package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pandeylakshya207-max/servemesh/internal/backend"
	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

func startMock(t *testing.T, id string) *router.Backend {
	t.Helper()
	srv := httptest.NewServer(backend.NewMock(backend.MockConfig{ID: id}).Handler())
	t.Cleanup(srv.Close)
	return router.NewBackend(id, strings.TrimPrefix(srv.URL, "http://"))
}

func startGateway(t *testing.T, p router.Policy, bs []*router.Backend) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(p, bs).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func chat(t *testing.T, gw *httptest.Server, rawBody string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(rawBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func chatJSON(prompt string) string {
	b, _ := json.Marshal(map[string]any{
		"model":      "mock",
		"stream":     true,
		"max_tokens": 3,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	return string(b)
}

func TestRoundRobinAcrossBackends(t *testing.T) {
	bs := []*router.Backend{startMock(t, "m0"), startMock(t, "m1")}
	gw := startGateway(t, router.NewRoundRobin(), bs)

	want := []string{"m0", "m1", "m0", "m1"}
	for i, w := range want {
		resp, body := chat(t, gw, chatJSON("hello there"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
		if got := resp.Header.Get("X-Backend-ID"); got != w {
			t.Fatalf("request %d: backend %s, want %s", i, got, w)
		}
		if resp.Header.Get("X-Gateway-Policy") != "round-robin" {
			t.Fatal("missing X-Gateway-Policy header")
		}
		if !strings.Contains(body, "data: [DONE]") {
			t.Fatalf("request %d: stream did not finish: %q", i, body)
		}
	}
}

func TestInFlightReleasedAfterRequest(t *testing.T) {
	b := startMock(t, "m0")
	gw := startGateway(t, router.NewLeastLoaded(), []*router.Backend{b})
	chat(t, gw, chatJSON("hello"))
	if got := b.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d after request, want 0", got)
	}
}

func TestNoHealthyBackendsReturns503(t *testing.T) {
	b := startMock(t, "m0")
	b.SetHealthy(false)
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{b})
	resp, _ := chat(t, gw, chatJSON("hello"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", resp.StatusCode)
	}
}

func TestBadRequestReturns400(t *testing.T) {
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{startMock(t, "m0")})
	for _, body := range []string{"not json", `{"model":"x","messages":[]}`} {
		resp, _ := chat(t, gw, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q: status %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestUpstreamDownReturns502(t *testing.T) {
	dead := router.NewBackend("dead", "127.0.0.1:1")
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{dead})
	resp, _ := chat(t, gw, chatJSON("hello"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if dead.InFlight() != 0 {
		t.Fatalf("in-flight = %d after failure, want 0", dead.InFlight())
	}
}

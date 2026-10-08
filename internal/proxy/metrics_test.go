package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

func scrape(t *testing.T, gwURL string) string {
	t.Helper()
	resp, err := http.Get(gwURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestMetricsReflectRequestsAndRetries(t *testing.T) {
	dead := router.NewBackend("dead", "127.0.0.1:1")
	good := startMock(t, "good")
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{dead, good})

	if resp, _ := chat(t, gw, chatJSON("hello")); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	out := scrape(t, gw.URL)
	for _, want := range []string{
		`servemesh_requests_total{backend="good",code="200"} 1`,
		`servemesh_first_chunk_seconds_count{backend="good"} 1`,
		`servemesh_pick_seconds_count 2`,
		`servemesh_retries_total 1`,
		`servemesh_backend_healthy{backend="dead"} 0`,
		`servemesh_backend_healthy{backend="good"} 1`,
		`servemesh_backend_in_flight{backend="good"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output is missing %q", want)
		}
	}
}

func TestMetricsCountRejectedRequests(t *testing.T) {
	gw := startGateway(t, router.NewRoundRobin(), []*router.Backend{startMock(t, "m")})
	if resp, _ := chat(t, gw, "not json"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	out := scrape(t, gw.URL)
	want := `servemesh_requests_total{backend="none",code="400"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("metrics output is missing %q", want)
	}
}

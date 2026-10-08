package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/admission"
	"github.com/pandeylakshya207-max/servemesh/internal/backend"
	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

func TestAdmissionShedsWhenSaturated(t *testing.T) {
	srv := httptest.NewServer(backend.NewMock(backend.MockConfig{ID: "m", DecodePerToken: 80 * time.Millisecond}).Handler())
	t.Cleanup(srv.Close)
	b := router.NewBackend("m", strings.TrimPrefix(srv.URL, "http://"))
	g := New(router.NewRoundRobin(), []*router.Backend{b})
	g.SetAdmission(admission.Config{MaxInFlight: 1, MaxQueue: 0})
	gw := httptest.NewServer(g.Handler())
	t.Cleanup(gw.Close)

	slow := `{"model":"m","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	firstDone := make(chan int, 1)
	go func() {
		resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(slow))
		if err != nil {
			t.Error(err)
			firstDone <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		firstDone <- resp.StatusCode
	}()

	deadline := time.Now().Add(2 * time.Second)
	for b.InFlight() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.InFlight() != 1 {
		t.Fatal("the first request never reached the backend")
	}

	resp, _ := chat(t, gw, chatJSON("second"))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request status %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}

	select {
	case code := <-firstDone:
		if code != http.StatusOK {
			t.Fatalf("first request status %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish")
	}

	out := scrape(t, gw.URL)
	for _, want := range []string{
		`servemesh_shed_total{priority="normal",reason="queue_full"} 1`,
		`servemesh_requests_total{backend="none",code="429"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output is missing %q", want)
		}
	}
}

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

func TestHealthCheckerEjectsAndRestoresWithHysteresis(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	b := router.NewBackend("b", strings.TrimPrefix(srv.URL, "http://"))
	hc := NewHealthChecker([]*router.Backend{b}, time.Hour, 500*time.Millisecond)
	ctx := context.Background()

	hc.CheckOnce(ctx)
	if !b.Healthy() {
		t.Fatal("healthy backend was ejected")
	}

	healthy.Store(false)
	hc.CheckOnce(ctx)
	if !b.Healthy() {
		t.Fatal("ejected after a single failed probe; want 2")
	}
	hc.CheckOnce(ctx)
	if b.Healthy() {
		t.Fatal("not ejected after 2 failed probes")
	}

	healthy.Store(true)
	hc.CheckOnce(ctx)
	if b.Healthy() {
		t.Fatal("restored after a single good probe; want 2")
	}
	hc.CheckOnce(ctx)
	if !b.Healthy() {
		t.Fatal("not restored after 2 good probes")
	}
}

func TestHealthCheckerDetectsDownServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	b := router.NewBackend("b", strings.TrimPrefix(srv.URL, "http://"))
	srv.Close()
	hc := NewHealthChecker([]*router.Backend{b}, time.Hour, 200*time.Millisecond)
	hc.CheckOnce(context.Background())
	hc.CheckOnce(context.Background())
	if b.Healthy() {
		t.Fatal("down server still marked healthy")
	}
}

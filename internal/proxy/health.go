package proxy

import (
	"context"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

// HealthChecker actively probes each backend's health endpoint. A backend is ejected
// after FailThreshold consecutive failures and restored after RiseThreshold
// consecutive successes (hysteresis, so one blip does not cause flapping).
type HealthChecker struct {
	backends      []*router.Backend
	client        *http.Client
	interval      time.Duration
	timeout       time.Duration
	FailThreshold int
	RiseThreshold int
	Path          string // health endpoint path; default /healthz

	// Touched only by CheckOnce's caller goroutine.
	fails map[*router.Backend]int
	rises map[*router.Backend]int
}

func NewHealthChecker(backends []*router.Backend, interval, timeout time.Duration) *HealthChecker {
	return &HealthChecker{
		backends:      backends,
		client:        &http.Client{},
		interval:      interval,
		timeout:       timeout,
		FailThreshold: 2,
		RiseThreshold: 2,
		Path:          "/healthz",
		fails:         make(map[*router.Backend]int),
		rises:         make(map[*router.Backend]int),
	}
}

func (h *HealthChecker) probe(ctx context.Context, b *router.Backend) bool {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+b.Addr+h.Path, nil)
	if err != nil {
		return false
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func (h *HealthChecker) update(b *router.Backend, ok bool) {
	if ok {
		h.fails[b] = 0
		if !b.Healthy() {
			h.rises[b]++
			if h.rises[b] >= h.RiseThreshold {
				b.SetHealthy(true)
				h.rises[b] = 0
				log.Printf("health: backend %s is healthy again", b.ID)
			}
		}
		return
	}
	h.rises[b] = 0
	h.fails[b]++
	if b.Healthy() && h.fails[b] >= h.FailThreshold {
		b.SetHealthy(false)
		log.Printf("health: backend %s ejected after %d failed probes", b.ID, h.fails[b])
	}
}

// CheckOnce probes every backend in parallel, then applies the results.
func (h *HealthChecker) CheckOnce(ctx context.Context) {
	ok := make([]bool, len(h.backends))
	var wg sync.WaitGroup
	for i, b := range h.backends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok[i] = h.probe(ctx, b)
		}()
	}
	wg.Wait()
	for i, b := range h.backends {
		h.update(b, ok[i])
	}
}

// Run probes immediately, then every interval, until ctx is cancelled.
func (h *HealthChecker) Run(ctx context.Context) {
	h.CheckOnce(ctx)
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.CheckOnce(ctx)
		}
	}
}

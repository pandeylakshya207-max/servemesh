package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/backend"
	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

// probePolicy records the in-flight count the backend had when Pick ran, and
// sleeps inside Pick to widen any race between choosing and reserving.
type probePolicy struct {
	mu   sync.Mutex
	seen []int64
}

func (p *probePolicy) Name() string { return "probe" }

func (p *probePolicy) Pick(_ *router.Request, bs []*router.Backend) (*router.Backend, error) {
	b := bs[0]
	p.mu.Lock()
	p.seen = append(p.seen, b.InFlight())
	p.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	return b, nil
}

func TestPickAndReserveAreAtomic(t *testing.T) {
	srv := httptest.NewServer(backend.NewMock(backend.MockConfig{ID: "m", DecodePerToken: 100 * time.Millisecond}).Handler())
	t.Cleanup(srv.Close)
	b := router.NewBackend("m", strings.TrimPrefix(srv.URL, "http://"))
	p := &probePolicy{}
	gw := startGateway(t, p, []*router.Backend{b})

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatJSON("hello")))
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) != n {
		t.Fatalf("Pick ran %d times, want %d", len(p.seen), n)
	}
	sort.Slice(p.seen, func(i, j int) bool { return p.seen[i] < p.seen[j] })
	for i, v := range p.seen {
		if v != int64(i) {
			t.Fatalf("in-flight seen by Pick = %v, want 0,1,2,3,4: the reservation is not atomic with the pick", p.seen)
		}
	}
	if b.InFlight() != 0 {
		t.Fatalf("in-flight = %d after all requests finished, want 0", b.InFlight())
	}
}

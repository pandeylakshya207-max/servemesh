package router

import (
	"strings"
	"testing"
)

func TestPrefixForgetsEjectedBackend(t *testing.T) {
	bs := makeBackends(3)
	p := NewPrefixAware(1000, 1.25)
	prompt := promptWith("g", 40, "q")
	home := pickFor(t, p, bs, prompt)

	home.SetHealthy(false)
	if other := pickFor(t, p, bs, prompt); other == home {
		t.Fatal("picked an unhealthy backend")
	}

	home.SetHealthy(true) // pretend it restarted with an empty cache
	if n := p.matched(home.ID, BlockHashes(strings.Fields(prompt))); n != 0 {
		t.Fatalf("stale cache entries kept for ejected backend: %d blocks", n)
	}
}

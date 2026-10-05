package router

import (
	"fmt"
	"testing"
)

func TestPrefixColdLeastLoadedTiesSpreadWhenIdle(t *testing.T) {
	bs := makeBackends(3)
	p := NewPrefixAware(100000, 5)
	p.ColdFallback = ColdLeastLoaded
	seen := map[string]int{}
	for g := 0; g < 40; g++ {
		seen[pickFor(t, p, bs, promptWith(fmt.Sprintf("idle%d", g), 40, "q")).ID]++
	}
	for _, b := range bs {
		if seen[b.ID] == 0 {
			t.Fatalf("idle cluster never used %s: %v", b.ID, seen)
		}
	}
}

func TestPrefixColdLeastLoadedStillAvoidsBusyBackends(t *testing.T) {
	bs := makeBackends(3)
	bs[0].Acquire()
	bs[0].Acquire()
	bs[1].Acquire()
	p := NewPrefixAware(100000, 5)
	p.ColdFallback = ColdLeastLoaded
	for g := 0; g < 20; g++ {
		if got := pickFor(t, p, bs, promptWith(fmt.Sprintf("busy%d", g), 40, "q")); got.ID != "b2" {
			t.Fatalf("prompt %d went to %s, want the least-loaded b2", g, got.ID)
		}
	}
}

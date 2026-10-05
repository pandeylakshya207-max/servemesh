package router

import "testing"

func TestPrefixColdFallbackLeastLoaded(t *testing.T) {
	bs := makeBackends(3)
	bs[0].Acquire()
	bs[0].Acquire()
	bs[1].Acquire()
	p := NewPrefixAware(1000, 5)
	p.ColdFallback = ColdLeastLoaded
	if got := pickFor(t, p, bs, promptWith("new", 40, "q")); got.ID != "b2" {
		t.Fatalf("got %s, want the idle backend b2", got.ID)
	}
}

func TestPrefixColdRendezvousIgnoresLoad(t *testing.T) {
	bs := makeBackends(3)
	prompt := promptWith("new", 40, "q")
	home := pickFor(t, NewPrefixAware(1000, 5), bs, prompt)

	home.Acquire() // make the home busier than the others
	home.Acquire()
	if got := pickFor(t, NewPrefixAware(1000, 5), bs, prompt); got != home {
		t.Fatalf("rendezvous placement moved from %s to %s because of load", home.ID, got.ID)
	}
}

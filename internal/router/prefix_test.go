package router

import (
	"fmt"
	"strings"
	"testing"
)

func promptWith(group string, n int, user string) string {
	ws := make([]string, n)
	for i := range ws {
		ws[i] = fmt.Sprintf("%s_%d", group, i)
	}
	return "system: " + strings.Join(ws, " ") + "\nuser: " + user + "\n"
}

func pickFor(t *testing.T, p Policy, bs []*Backend, prompt string) *Backend {
	t.Helper()
	b, err := p.Pick(&Request{Prompt: prompt}, bs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPrefixSamePromptSticks(t *testing.T) {
	bs := makeBackends(3)
	p := NewPrefixAware(100, 1.25)
	prompt := promptWith("g1", 40, "hello")
	first := pickFor(t, p, bs, prompt)
	for i := 0; i < 5; i++ {
		if got := pickFor(t, p, bs, prompt); got != first {
			t.Fatalf("request %d went to %s, want %s", i, got.ID, first.ID)
		}
	}
}

func TestPrefixSharedSystemPromptSticks(t *testing.T) {
	bs := makeBackends(3)
	p := NewPrefixAware(100, 1.25)
	a := pickFor(t, p, bs, promptWith("g1", 40, "alpha beta"))
	b := pickFor(t, p, bs, promptWith("g1", 40, "gamma delta epsilon"))
	if a != b {
		t.Fatalf("shared prefix split across %s and %s", a.ID, b.ID)
	}
}

func TestPrefixSpreadsGroupsAcrossBackends(t *testing.T) {
	bs := makeBackends(3)
	p := NewPrefixAware(10000, 1.25)
	counts := map[string]int{}
	for g := 0; g < 40; g++ {
		counts[pickFor(t, p, bs, promptWith(fmt.Sprintf("grp%d", g), 40, "q")).ID]++
	}
	for _, b := range bs {
		if counts[b.ID] == 0 {
			t.Fatalf("backend %s received no groups: %v", b.ID, counts)
		}
	}
}

func TestPrefixSkipsUnhealthy(t *testing.T) {
	bs := makeBackends(3)
	bs[0].SetHealthy(false)
	p := NewPrefixAware(1000, 1.25)
	for g := 0; g < 20; g++ {
		if b := pickFor(t, p, bs, promptWith(fmt.Sprintf("grp%d", g), 40, "q")); b.ID == "b0" {
			t.Fatal("routed to unhealthy backend")
		}
	}
}

func TestPrefixLoadBoundSpills(t *testing.T) {
	bs := makeBackends(3)
	p := NewPrefixAware(1000, 1.25)
	prompt := promptWith("hot", 40, "q")
	home := pickFor(t, p, bs, prompt)
	for i := 0; i < 10; i++ {
		home.Acquire()
	}
	if got := pickFor(t, p, bs, prompt); got == home {
		t.Fatalf("overloaded backend %s was still chosen", home.ID)
	}
}

func TestPrefixShortPromptUsesLeastLoaded(t *testing.T) {
	bs := makeBackends(3)
	bs[0].Acquire()
	bs[1].Acquire()
	p := NewPrefixAware(100, 1.25)
	if got := pickFor(t, p, bs, "user: hi\n"); got.ID != "b2" {
		t.Fatalf("got %s, want b2", got.ID)
	}
}

func TestPrefixTrackerIsBounded(t *testing.T) {
	bs := makeBackends(2)
	p := NewPrefixAware(4, 1.25)
	for i := 0; i < 50; i++ {
		pickFor(t, p, bs, promptWith(fmt.Sprintf("u%d", i), 40, "q"))
	}
	for id, s := range p.sets {
		if s.lru.Len() > 4 {
			t.Fatalf("backend %s tracker holds %d blocks, cap is 4", id, s.lru.Len())
		}
	}
}

func TestPrefixNoBackends(t *testing.T) {
	bs := makeBackends(1)
	bs[0].SetHealthy(false)
	if _, err := NewPrefixAware(10, 1.25).Pick(&Request{Prompt: "x"}, bs); err != ErrNoBackends {
		t.Fatalf("got %v, want ErrNoBackends", err)
	}
}

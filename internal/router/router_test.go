package router

import (
	"fmt"
	"sync"
	"testing"
)

func makeBackends(n int) []*Backend {
	bs := make([]*Backend, n)
	for i := range bs {
		bs[i] = NewBackend(fmt.Sprintf("b%d", i), fmt.Sprintf("localhost:%d", 9000+i))
	}
	return bs
}

func pickIDs(t *testing.T, p Policy, bs []*Backend, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b, err := p.Pick(&Request{}, bs)
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		ids = append(ids, b.ID)
	}
	return ids
}

func TestRoundRobinCycles(t *testing.T) {
	got := pickIDs(t, NewRoundRobin(), makeBackends(3), 6)
	want := []string{"b0", "b1", "b2", "b0", "b1", "b2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	bs := makeBackends(3)
	bs[1].SetHealthy(false)
	got := pickIDs(t, NewRoundRobin(), bs, 4)
	want := []string{"b0", "b2", "b0", "b2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestLeastLoadedPicksIdleBackend(t *testing.T) {
	bs := makeBackends(3)
	bs[0].Acquire()
	bs[0].Acquire()
	bs[1].Acquire()
	p := NewLeastLoaded()

	b, err := p.Pick(&Request{}, bs)
	if err != nil || b.ID != "b2" {
		t.Fatalf("got %v, %v; want b2", b, err)
	}

	bs[2].Acquire()
	bs[2].Acquire()
	bs[2].Acquire()
	b, _ = p.Pick(&Request{}, bs)
	if b.ID != "b1" {
		t.Fatalf("got %s; want b1", b.ID)
	}
}

func TestLeastLoadedSkipsUnhealthy(t *testing.T) {
	bs := makeBackends(2)
	bs[0].SetHealthy(false)
	bs[1].Acquire()
	b, err := NewLeastLoaded().Pick(&Request{}, bs)
	if err != nil || b.ID != "b1" {
		t.Fatalf("got %v, %v; want b1", b, err)
	}
}

func TestNoHealthyBackends(t *testing.T) {
	bs := makeBackends(2)
	bs[0].SetHealthy(false)
	bs[1].SetHealthy(false)
	for _, p := range []Policy{NewRoundRobin(), NewLeastLoaded()} {
		if _, err := p.Pick(&Request{}, bs); err != ErrNoBackends {
			t.Fatalf("%s: got %v, want ErrNoBackends", p.Name(), err)
		}
	}
}

func TestRoundRobinConcurrent(t *testing.T) {
	bs := makeBackends(4)
	p := NewRoundRobin()
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Pick(&Request{}, bs); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

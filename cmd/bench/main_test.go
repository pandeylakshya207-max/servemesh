package main

import (
	"math/rand"
	"testing"
)

func TestPercentile(t *testing.T) {
	s := make([]float64, 100)
	for i := range s {
		s[i] = float64(i + 1)
	}
	if got := percentile(s, 0.50); got != 50 {
		t.Fatalf("p50 = %v, want 50", got)
	}
	if got := percentile(s, 0.99); got != 99 {
		t.Fatalf("p99 = %v, want 99", got)
	}
	if got := percentile(s, 1.0); got != 100 {
		t.Fatalf("p100 = %v, want 100", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("empty = %v, want 0", got)
	}
}

func TestSharedWorkloadUsesFixedSystemPrompts(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	wl := newShared(rng, 3, 50, 10)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		m := wl.Next(rng)
		if len(m) != 2 || m[0].Role != "system" || m[1].Role != "user" {
			t.Fatalf("unexpected shape: %+v", m)
		}
		seen[m[0].Content] = true
	}
	if len(seen) != 3 {
		t.Fatalf("saw %d distinct system prompts, want 3", len(seen))
	}
}

func TestMultiTurnHistoryIsPrefix(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	wl := newMultiTurn(rng, 1, 20, 10, 10)
	first := wl.Next(rng)
	second := wl.Next(rng)
	if len(second) != len(first)+2 {
		t.Fatalf("len(second)=%d, want %d", len(second), len(first)+2)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("message %d changed between turns", i)
		}
	}
}

func TestSameSeedSameWorkload(t *testing.T) {
	a := newShared(rand.New(rand.NewSource(7)), 4, 30, 5)
	b := newShared(rand.New(rand.NewSource(7)), 4, 30, 5)
	for i := range a.systems {
		if a.systems[i] != b.systems[i] {
			t.Fatal("same seed produced different system prompts")
		}
	}
}

package main

import (
	"testing"
	"time"
)

func TestWithinSLO(t *testing.T) {
	s := []sample{
		{class: "high", status: "ok", ttft: 50 * time.Millisecond},
		{class: "high", status: "ok", ttft: 2 * time.Second},
		{class: "high", status: "shed"},
		{class: "low", status: "ok", ttft: 10 * time.Millisecond},
	}
	if g, n := withinSLO(s, "high", time.Second); g != 1 || n != 3 {
		t.Fatalf("high: %d of %d, want 1 of 3", g, n)
	}
	if g, n := withinSLO(s, "low", time.Second); g != 1 || n != 1 {
		t.Fatalf("low: %d of %d, want 1 of 1", g, n)
	}
}

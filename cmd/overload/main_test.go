package main

import (
	"testing"
	"time"
)

func TestSummarizeCountsClassesAndOutcomes(t *testing.T) {
	samples := []sample{
		{class: "high", status: "ok", ttft: 10 * time.Millisecond, e2e: 100 * time.Millisecond},
		{class: "high", status: "shed"},
		{class: "low", status: "error"},
		{class: "low", status: "ok", ttft: 30 * time.Millisecond, e2e: 300 * time.Millisecond},
	}
	h := summarize(samples, "high", 10)
	if h.Sent != 2 || h.OK != 1 || h.Shed != 1 || h.Errors != 0 {
		t.Fatalf("high: %+v", h)
	}
	if h.TTFTp50 != 10 || h.E2Ep50 != 100 {
		t.Fatalf("high latencies: ttft=%v e2e=%v, want 10 and 100", h.TTFTp50, h.E2Ep50)
	}
	l := summarize(samples, "low", 10)
	if l.Sent != 2 || l.OK != 1 || l.Shed != 0 || l.Errors != 1 {
		t.Fatalf("low: %+v", l)
	}
}

func TestPct(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if pct(s, 0.5) != 5 || pct(s, 0.95) != 10 || pct(nil, 0.5) != 0 {
		t.Fatalf("pct is wrong: p50=%v p95=%v", pct(s, 0.5), pct(s, 0.95))
	}
}

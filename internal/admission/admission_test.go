package admission

import (
	"context"
	"errors"
	"testing"
	"time"
)

type result struct {
	rel func()
	err error
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func recv(t *testing.T, ch chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an Acquire result")
		return result{}
	}
}

func acquireAsync(c *Controller, ctx context.Context, p Priority) chan result {
	ch := make(chan result, 1)
	go func() {
		rel, err := c.Acquire(ctx, p)
		ch <- result{rel, err}
	}()
	return ch
}

func TestAdmitsUpToLimitThenQueues(t *testing.T) {
	c := New(Config{MaxInFlight: 2, MaxQueue: 4})
	ctx := context.Background()
	r1, err := c.Acquire(ctx, Normal)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.Acquire(ctx, Normal)
	if err != nil {
		t.Fatal(err)
	}
	if c.InFlight() != 2 {
		t.Fatalf("in-flight = %d, want 2", c.InFlight())
	}
	ch := acquireAsync(c, ctx, Normal)
	waitFor(t, func() bool { return c.Queued() == 1 })
	select {
	case <-ch:
		t.Fatal("admitted beyond the limit")
	default:
	}
	r1()
	r3 := recv(t, ch)
	if r3.err != nil {
		t.Fatal(r3.err)
	}
	if c.InFlight() != 2 || c.Queued() != 0 {
		t.Fatalf("in-flight=%d queued=%d, want 2 and 0", c.InFlight(), c.Queued())
	}
	r2()
	r3.rel()
	if c.InFlight() != 0 {
		t.Fatalf("in-flight = %d after releasing everything", c.InFlight())
	}
}

func TestPriorityOrderAndFIFOWithinClass(t *testing.T) {
	c := New(Config{MaxInFlight: 1, MaxQueue: 10})
	ctx := context.Background()
	hold, err := c.Acquire(ctx, Normal)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 10)
	enqueue := func(name string, p Priority) {
		before := c.Queued()
		go func() {
			rel, err := c.Acquire(ctx, p)
			if err != nil {
				order <- "err:" + name
				return
			}
			order <- name
			rel()
		}()
		waitFor(t, func() bool { return c.Queued() == before+1 })
	}
	enqueue("low1", Low)
	enqueue("normal1", Normal)
	enqueue("high1", High)
	enqueue("normal2", Normal)
	enqueue("high2", High)
	hold()
	for _, want := range []string{"high1", "high2", "normal1", "normal2", "low1"} {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("admitted %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func TestQueueFullShedsNewcomerOfEqualOrLowerPriority(t *testing.T) {
	c := New(Config{MaxInFlight: 1, MaxQueue: 1})
	ctx := context.Background()
	hold, _ := c.Acquire(ctx, Normal)
	queued := acquireAsync(c, ctx, Normal)
	waitFor(t, func() bool { return c.Queued() == 1 })
	for _, p := range []Priority{Normal, Low} {
		if _, err := c.Acquire(ctx, p); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("priority %s: err = %v, want ErrQueueFull", p, err)
		}
	}
	hold()
	r := recv(t, queued)
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.rel()
}

func TestHigherPriorityEvictsNewestLowerPriorityWaiter(t *testing.T) {
	c := New(Config{MaxInFlight: 1, MaxQueue: 1})
	ctx := context.Background()
	hold, _ := c.Acquire(ctx, Normal)
	low := acquireAsync(c, ctx, Low)
	waitFor(t, func() bool { return c.Queued() == 1 })
	high := acquireAsync(c, ctx, High)
	if lr := recv(t, low); !errors.Is(lr.err, ErrEvicted) || !errors.Is(lr.err, ErrShed) {
		t.Fatalf("low-priority waiter: err = %v, want ErrEvicted", lr.err)
	}
	if c.Queued() != 1 {
		t.Fatalf("queued = %d, want 1 (the high-priority request)", c.Queued())
	}
	hold()
	hr := recv(t, high)
	if hr.err != nil {
		t.Fatal(hr.err)
	}
	hr.rel()
}

func TestQueueWaitBudgetTimesOut(t *testing.T) {
	c := New(Config{MaxInFlight: 1, MaxQueue: 5, MaxWait: [3]time.Duration{time.Second, 40 * time.Millisecond, time.Second}})
	ctx := context.Background()
	hold, _ := c.Acquire(ctx, Normal)
	start := time.Now()
	_, err := c.Acquire(ctx, Normal)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed < 30*time.Millisecond || elapsed > time.Second {
		t.Fatalf("waited %v, expected about 40ms", elapsed)
	}
	if c.Queued() != 0 {
		t.Fatalf("queued = %d after a timeout, want 0", c.Queued())
	}
	hold()
}

func TestCancelledWaiterIsRemoved(t *testing.T) {
	c := New(Config{MaxInFlight: 1, MaxQueue: 5})
	hold, _ := c.Acquire(context.Background(), Normal)
	ctx, cancel := context.WithCancel(context.Background())
	ch := acquireAsync(c, ctx, Normal)
	waitFor(t, func() bool { return c.Queued() == 1 })
	cancel()
	r := recv(t, ch)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", r.err)
	}
	if c.Queued() != 0 {
		t.Fatalf("queued = %d after cancel, want 0", c.Queued())
	}
	hold()
	if c.InFlight() != 0 {
		t.Fatalf("in-flight = %d, want 0", c.InFlight())
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	c := New(Config{MaxInFlight: 2, MaxQueue: 1})
	r, err := c.Acquire(context.Background(), Normal)
	if err != nil {
		t.Fatal(err)
	}
	r()
	r()
	if c.InFlight() != 0 {
		t.Fatalf("in-flight = %d after a double release, want 0", c.InFlight())
	}
}

func TestDisabledAdmitsEverything(t *testing.T) {
	c := New(Config{})
	for i := 0; i < 100; i++ {
		rel, err := c.Acquire(context.Background(), Low)
		if err != nil {
			t.Fatal(err)
		}
		_ = rel
	}
	if c.InFlight() != 0 || c.Queued() != 0 {
		t.Fatal("a disabled controller must not account for requests")
	}
}

func TestParsePriorityAndReason(t *testing.T) {
	cases := map[string]Priority{"high": High, "HIGH": High, " low ": Low, "": Normal, "bogus": Normal, "normal": Normal}
	for in, want := range cases {
		if got := ParsePriority(in); got != want {
			t.Errorf("ParsePriority(%q) = %v, want %v", in, got, want)
		}
	}
	if Reason(ErrQueueFull) != "queue_full" || Reason(ErrEvicted) != "evicted" || Reason(ErrTimeout) != "timeout" {
		t.Fatal("Reason labels are wrong")
	}
}

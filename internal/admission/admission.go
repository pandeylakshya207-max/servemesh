// Package admission implements priority-aware admission control: a bound on
// concurrent requests, a bounded priority queue with per-class wait budgets, and
// load shedding when a budget cannot be met.
package admission

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Priority is a request class; a lower value is served first.
type Priority int

const (
	High Priority = iota
	Normal
	Low
	numPriorities
)

func (p Priority) String() string {
	switch p {
	case High:
		return "high"
	case Low:
		return "low"
	}
	return "normal"
}

// ParsePriority maps a header value to a priority; anything unknown is Normal.
func ParsePriority(s string) Priority {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return High
	case "low":
		return Low
	}
	return Normal
}

var (
	// ErrShed is wrapped by every error that means the request was not admitted for load reasons.
	ErrShed      = errors.New("admission: request shed")
	ErrQueueFull = fmt.Errorf("%w: queue full", ErrShed)
	ErrEvicted   = fmt.Errorf("%w: evicted by a higher-priority request", ErrShed)
	ErrTimeout   = fmt.Errorf("%w: waited longer than its budget", ErrShed)
)

// Reason is a short label for a shed error, for metrics.
func Reason(err error) string {
	switch {
	case errors.Is(err, ErrQueueFull):
		return "queue_full"
	case errors.Is(err, ErrEvicted):
		return "evicted"
	case errors.Is(err, ErrTimeout):
		return "timeout"
	}
	return "other"
}

// Config sets the limits. MaxInFlight <= 0 disables admission control entirely.
type Config struct {
	MaxInFlight int
	MaxQueue    int
	// MaxWait is how long a queued request of each class may wait, indexed by
	// Priority. Zero means no time limit (wait until admitted, evicted or cancelled).
	MaxWait [3]time.Duration
}

type waiter struct {
	prio     Priority
	done     chan error // buffered; receives nil when admitted
	resolved bool       // set under the lock when admitted or evicted
}

// Controller admits requests up to a concurrency limit and queues the rest.
type Controller struct {
	cfg      Config
	mu       sync.Mutex
	inFlight int
	queued   int
	queues   [numPriorities][]*waiter
}

func New(cfg Config) *Controller { return &Controller{cfg: cfg} }

func (c *Controller) InFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight
}

func (c *Controller) Queued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queued
}

// Acquire admits the request, queues it, or sheds it. On success the returned
// function must be called exactly once (extra calls are ignored) to release the slot.
func (c *Controller) Acquire(ctx context.Context, p Priority) (func(), error) {
	if c.cfg.MaxInFlight <= 0 {
		return func() {}, nil
	}
	if p < High || p > Low {
		p = Normal
	}

	c.mu.Lock()
	if c.inFlight < c.cfg.MaxInFlight && c.queued == 0 {
		c.inFlight++
		c.mu.Unlock()
		return c.releaser(), nil
	}
	if c.queued >= c.cfg.MaxQueue {
		victim := c.lowestBelowLocked(p)
		if victim == nil {
			c.mu.Unlock()
			return nil, ErrQueueFull
		}
		c.removeLocked(victim)
		victim.resolved = true
		victim.done <- ErrEvicted
	}
	w := &waiter{prio: p, done: make(chan error, 1)}
	c.queues[p] = append(c.queues[p], w)
	c.queued++
	c.dispatchLocked() // grants immediately if there is spare capacity
	c.mu.Unlock()

	var timeout <-chan time.Time
	if d := c.cfg.MaxWait[p]; d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case err := <-w.done:
		if err != nil {
			return nil, err
		}
		return c.releaser(), nil
	case <-timeout:
		return c.abandon(w, ErrTimeout)
	case <-ctx.Done():
		return c.abandon(w, ctx.Err())
	}
}

// abandon removes a waiter that timed out or was cancelled, unless a grant or
// eviction got there first, in which case that outcome wins.
func (c *Controller) abandon(w *waiter, cause error) (func(), error) {
	c.mu.Lock()
	if w.resolved {
		c.mu.Unlock()
		if err := <-w.done; err != nil {
			return nil, err
		}
		return c.releaser(), nil
	}
	c.removeLocked(w)
	c.mu.Unlock()
	return nil, cause
}

func (c *Controller) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.inFlight--
			c.dispatchLocked()
			c.mu.Unlock()
		})
	}
}

// dispatchLocked hands free slots to waiters, highest priority first, FIFO within a class.
func (c *Controller) dispatchLocked() {
	for c.inFlight < c.cfg.MaxInFlight {
		w := c.popLocked()
		if w == nil {
			return
		}
		c.inFlight++
		w.resolved = true
		w.done <- nil
	}
}

func (c *Controller) popLocked() *waiter {
	for p := High; p < numPriorities; p++ {
		if q := c.queues[p]; len(q) > 0 {
			w := q[0]
			q[0] = nil
			c.queues[p] = q[1:]
			c.queued--
			return w
		}
	}
	return nil
}

func (c *Controller) removeLocked(w *waiter) {
	q := c.queues[w.prio]
	for i, x := range q {
		if x == w {
			copy(q[i:], q[i+1:])
			q[len(q)-1] = nil
			c.queues[w.prio] = q[:len(q)-1]
			c.queued--
			return
		}
	}
}

// lowestBelowLocked returns the newest waiter of the lowest non-empty class that is
// strictly lower priority than p (it has waited the least, so evicting it wastes the least).
func (c *Controller) lowestBelowLocked(p Priority) *waiter {
	for q := Low; q > p; q-- {
		if n := len(c.queues[q]); n > 0 {
			return c.queues[q][n-1]
		}
	}
	return nil
}

package router

import (
	"errors"
	"sync/atomic"
)

// ErrNoBackends is returned when no healthy backend is available.
var ErrNoBackends = errors.New("router: no healthy backends available")

// Request carries what a routing policy needs to make a decision.
type Request struct {
	ID     string
	Model  string
	Prompt string
}

// Backend is one model-serving replica. Always used via pointer: it holds atomics.
type Backend struct {
	ID        string
	Addr      string
	inFlight  atomic.Int64
	unhealthy atomic.Bool
}

func NewBackend(id, addr string) *Backend {
	return &Backend{ID: id, Addr: addr}
}

func (b *Backend) InFlight() int64   { return b.inFlight.Load() }
func (b *Backend) Acquire()          { b.inFlight.Add(1) }
func (b *Backend) Release()          { b.inFlight.Add(-1) }
func (b *Backend) Healthy() bool     { return !b.unhealthy.Load() }
func (b *Backend) SetHealthy(h bool) { b.unhealthy.Store(!h) }

// Policy decides which backend serves a request.
type Policy interface {
	Name() string
	Pick(req *Request, backends []*Backend) (*Backend, error)
}

func healthy(backends []*Backend) []*Backend {
	out := make([]*Backend, 0, len(backends))
	for _, b := range backends {
		if b.Healthy() {
			out = append(out, b)
		}
	}
	return out
}

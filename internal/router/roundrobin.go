package router

import "sync/atomic"

// RoundRobin cycles through healthy backends in order.
type RoundRobin struct {
	next atomic.Uint64
}

func NewRoundRobin() *RoundRobin { return &RoundRobin{} }

func (p *RoundRobin) Name() string { return "round-robin" }

func (p *RoundRobin) Pick(_ *Request, backends []*Backend) (*Backend, error) {
	h := healthy(backends)
	if len(h) == 0 {
		return nil, ErrNoBackends
	}
	i := p.next.Add(1) - 1
	return h[i%uint64(len(h))], nil
}

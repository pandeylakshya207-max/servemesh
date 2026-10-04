package router

// LeastLoaded picks the healthy backend with the fewest in-flight requests.
// Ties go to the earliest backend in the list, which keeps behaviour deterministic.
type LeastLoaded struct{}

func NewLeastLoaded() *LeastLoaded { return &LeastLoaded{} }

func (p *LeastLoaded) Name() string { return "least-loaded" }

func (p *LeastLoaded) Pick(_ *Request, backends []*Backend) (*Backend, error) {
	var best *Backend
	for _, b := range backends {
		if !b.Healthy() {
			continue
		}
		if best == nil || b.InFlight() < best.InFlight() {
			best = b
		}
	}
	if best == nil {
		return nil, ErrNoBackends
	}
	return best, nil
}

package proxy

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

// recorder remembers the status code written to the client.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type metrics struct {
	reg        *prometheus.Registry
	requests   *prometheus.CounterVec
	firstChunk *prometheus.HistogramVec
	pick       prometheus.Histogram
}

var (
	inFlightDesc = prometheus.NewDesc("servemesh_backend_in_flight",
		"Requests currently in flight to the backend.", []string{"backend"}, nil)
	healthyDesc = prometheus.NewDesc("servemesh_backend_healthy",
		"1 if the backend is considered healthy, 0 otherwise.", []string{"backend"}, nil)
)

// backendCollector reports live per-backend state at scrape time.
type backendCollector struct{ g *Gateway }

func (c *backendCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- inFlightDesc
	ch <- healthyDesc
}

func (c *backendCollector) Collect(ch chan<- prometheus.Metric) {
	for _, b := range c.g.backends {
		ch <- prometheus.MustNewConstMetric(inFlightDesc, prometheus.GaugeValue, float64(b.InFlight()), b.ID)
		h := 0.0
		if b.Healthy() {
			h = 1
		}
		ch <- prometheus.MustNewConstMetric(healthyDesc, prometheus.GaugeValue, h, b.ID)
	}
}

func newMetrics(g *Gateway) *metrics {
	m := &metrics{reg: prometheus.NewRegistry()}
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "servemesh_requests_total",
		Help: "Chat requests by the backend that finished them (none if no backend did) and HTTP status code (499 if the client left before anything was written).",
	}, []string{"backend", "code"})
	m.firstChunk = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "servemesh_first_chunk_seconds",
		Help:    "Time from request arrival to the first response chunk written to the client.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"backend"})
	m.pick = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "servemesh_pick_seconds",
		Help:    "Time spent choosing and reserving a backend, including waiting for the pick lock.",
		Buckets: []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1},
	})
	m.reg.MustRegister(
		m.requests, m.firstChunk, m.pick,
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "servemesh_retries_total", Help: "Requests retried on a different backend before the first byte.",
		}, func() float64 { return float64(g.retries.Load()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "servemesh_midstream_failures_total", Help: "Streams that broke after the first byte.",
		}, func() float64 { return float64(g.midStreamFailures.Load()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "servemesh_upstream_timeouts_total", Help: "Requests answered with 504 because a backend did not start responding in time.",
		}, func() float64 { return float64(g.upstreamTimeouts.Load()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "servemesh_stale_conn_retries_total", Help: "Requests retried on a fresh connection after a stale pooled connection.",
		}, func() float64 { return float64(g.staleRetries.Load()) }),
		&backendCollector{g: g},
	)
	return m
}

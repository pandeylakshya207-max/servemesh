package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pandeylakshya207-max/servemesh/internal/admission"
	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

const maxBodyBytes = 4 << 20 // 4 MiB

var doneMarker = []byte("[DONE]")

// Gateway routes OpenAI-style chat requests to backends and streams responses back.
type Gateway struct {
	policy   router.Policy
	backends []*router.Backend
	client   *http.Client // pooled connections
	fresh    *http.Client // new connection per request; used to retry a stale pooled connection
	mux      *http.ServeMux
	seq      atomic.Uint64

	// MaxAttempts is how many different backends one request may try before the
	// first response byte reaches the client. Default 3.
	MaxAttempts int

	retries           atomic.Int64
	midStreamFailures atomic.Int64
	upstreamTimeouts  atomic.Int64
	staleRetries      atomic.Int64

	// pickMu makes choosing a backend and reserving a slot on it one atomic step,
	// so simultaneous requests cannot all pick the same least-loaded backend.
	pickMu sync.Mutex

	metrics *metrics

	// admit limits concurrent requests and sheds load; disabled unless SetAdmission is called.
	admit *admission.Controller
}

func New(policy router.Policy, backends []*router.Backend) *Gateway {
	g := &Gateway{
		policy:      policy,
		backends:    backends,
		MaxAttempts: 3,
		// No overall Timeout: it would cut off long streams.
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: 256,
			// Keep this below the backends' keep-alive timeout (llama-server advertises
			// 5 s); otherwise a request can race with the backend closing an idle connection.
			IdleConnTimeout:       4 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
		fresh: &http.Client{Transport: &http.Transport{
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
		mux: http.NewServeMux(),
	}
	g.admit = admission.New(admission.Config{})
	g.metrics = newMetrics(g)
	g.mux.HandleFunc("POST /v1/chat/completions", g.handleChat)
	g.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	g.mux.HandleFunc("GET /backends", g.handleBackends)
	g.mux.Handle("GET /metrics", promhttp.HandlerFor(g.metrics.reg, promhttp.HandlerOpts{}))
	return g
}

func (g *Gateway) Handler() http.Handler { return g.mux }

// SetResponseHeaderTimeout sets how long the gateway waits for a backend to start
// responding. With a queueing backend (for example a saturated llama-server) this
// bounds time-to-first-byte.
func (g *Gateway) SetResponseHeaderTimeout(d time.Duration) {
	g.client.Transport.(*http.Transport).ResponseHeaderTimeout = d
	g.fresh.Transport.(*http.Transport).ResponseHeaderTimeout = d
}

func (g *Gateway) maxAttempts() int {
	if g.MaxAttempts > 0 {
		return g.MaxAttempts
	}
	return 3
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// contentText handles both string content and array-of-parts content.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func (g *Gateway) untried(tried map[*router.Backend]bool) []*router.Backend {
	out := make([]*router.Backend, 0, len(g.backends))
	for _, b := range g.backends {
		if !tried[b] {
			out = append(out, b)
		}
	}
	return out
}

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &recorder{ResponseWriter: w}
	served := "none"
	defer func() {
		code := rec.status
		if code == 0 {
			code = 499 // nothing was written: the client went away first
		}
		g.metrics.requests.WithLabelValues(served, strconv.Itoa(code)).Inc()
	}()

	body, err := io.ReadAll(http.MaxBytesReader(rec, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(rec, "request body unreadable or too large", http.StatusBadRequest)
		return
	}
	var cr chatRequest
	if err := json.Unmarshal(body, &cr); err != nil || len(cr.Messages) == 0 {
		http.Error(rec, "invalid chat request", http.StatusBadRequest)
		return
	}
	var sb strings.Builder
	for _, m := range cr.Messages {
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(contentText(m.Content))
		sb.WriteString("\n")
	}
	req := &router.Request{
		ID:     fmt.Sprintf("req-%d", g.seq.Add(1)),
		Model:  cr.Model,
		Prompt: sb.String(),
	}

	prio := admission.ParsePriority(r.Header.Get("X-Priority"))
	waitStart := time.Now()
	release, aerr := g.admit.Acquire(r.Context(), prio)
	if aerr != nil {
		if r.Context().Err() != nil {
			return // the client left while waiting; recorded as 499
		}
		g.metrics.shed.WithLabelValues(prio.String(), admission.Reason(aerr)).Inc()
		rec.Header().Set("Retry-After", "1")
		http.Error(rec, "server busy: request shed", http.StatusTooManyRequests)
		return
	}
	defer release()
	g.metrics.admitWait.Observe(time.Since(waitStart).Seconds())

	tried := make(map[*router.Backend]bool)
	attempts := 0
	var pickErr error
	for attempts < g.maxAttempts() {
		t0 := time.Now()
		g.pickMu.Lock()
		b, perr := g.policy.Pick(req, g.untried(tried))
		if perr == nil {
			b.Acquire() // reserve under the lock; tryBackend releases it
		}
		g.pickMu.Unlock()
		g.metrics.pick.Observe(time.Since(t0).Seconds())
		if perr != nil {
			pickErr = perr
			break
		}
		tried[b] = true
		attempts++
		if attempts > 1 {
			g.retries.Add(1)
		}
		if g.tryBackend(rec, r, b, body, req.ID, attempts, start) {
			served = b.ID
			return
		}
	}
	if attempts == 0 {
		if errors.Is(pickErr, router.ErrNoBackends) {
			http.Error(rec, "no healthy backends", http.StatusServiceUnavailable)
		} else {
			http.Error(rec, "routing error", http.StatusInternalServerError)
		}
		return
	}
	http.Error(rec, "all upstream attempts failed", http.StatusBadGateway)
}

// isConnReset reports whether err looks like the peer closing or resetting a connection.
func isConnReset(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "forcibly closed")
}

// do sends the request to one backend. If the failure looks like a stale pooled
// connection (a reused connection that died before any response byte), it retries
// once on a fresh connection to the same backend; Go does not retry a POST itself.
func (g *Gateway) do(ctx context.Context, b *router.Backend, body []byte, reqID string) (*http.Response, error) {
	send := func(c *http.Client) (*http.Response, bool, error) {
		var reused atomic.Bool
		trace := &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) },
		}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost,
			"http://"+b.Addr+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		return resp, reused.Load(), err
	}

	resp, reused, err := send(g.client)
	if err != nil && reused && ctx.Err() == nil && isConnReset(err) {
		g.staleRetries.Add(1)
		log.Printf("%s: backend %s: pooled connection was stale (%v); retrying on a fresh connection", reqID, b.ID, err)
		resp, _, err = send(g.fresh)
	}
	return resp, err
}

// tryBackend returns true once the response is committed to the client (success,
// a client-side error, a timeout, or a mid-stream failure) and false if the attempt
// failed before any byte reached the client, so the caller may retry elsewhere.
// The caller has already reserved a slot on b; tryBackend releases it.
func (g *Gateway) tryBackend(w http.ResponseWriter, r *http.Request, b *router.Backend, body []byte, reqID string, attempt int, start time.Time) bool {
	defer b.Release()

	resp, err := g.do(r.Context(), b, body, reqID)
	if err != nil {
		if r.Context().Err() != nil {
			return true // the client went away; not the backend's fault
		}
		if strings.Contains(err.Error(), "awaiting response headers") {
			// Slow, not dead: do not eject it and do not retry (a retry would add
			// load to an overloaded cluster). Active health checks decide ejection.
			g.upstreamTimeouts.Add(1)
			log.Printf("%s: backend %s timed out waiting for a response", reqID, b.ID)
			http.Error(w, "upstream timeout", http.StatusGatewayTimeout)
			return true
		}
		log.Printf("%s: attempt %d: backend %s unreachable: %v", reqID, attempt, b.ID, err)
		b.SetHealthy(false)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		log.Printf("%s: attempt %d: backend %s returned %d", reqID, attempt, b.ID, resp.StatusCode)
		b.SetHealthy(false)
		return false
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Gateway-Policy", g.policy.Name())
	w.Header().Set("X-Gateway-Attempts", strconv.Itoa(attempt))
	w.WriteHeader(resp.StatusCode)

	isSSE := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	tail := make([]byte, 0, 64)
	sawDone := false
	failed := false
	gotFirst := false
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return true // client went away
			}
			if !gotFirst {
				gotFirst = true
				g.metrics.firstChunk.WithLabelValues(b.ID).Observe(time.Since(start).Seconds())
			}
			if flusher != nil {
				flusher.Flush()
			}
			if isSSE {
				tail = append(tail, buf[:n]...)
				if bytes.Contains(tail, doneMarker) {
					sawDone = true
				}
				if len(tail) > 32 {
					tail = tail[len(tail)-32:]
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				failed = isSSE && !sawDone // clean close, but the stream never finished
			} else {
				failed = true
			}
			break
		}
	}

	if r.Context().Err() != nil {
		return true // client disconnect, not a backend failure
	}
	if failed {
		g.midStreamFailures.Add(1)
		b.SetHealthy(false)
		log.Printf("%s: backend %s failed mid-stream", reqID, b.ID)
		if isSSE {
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"upstream failed mid-stream\",\"type\":\"upstream_error\"}}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		} else {
			panic(http.ErrAbortHandler) // abort so the client sees an error, not a silent truncation
		}
	}
	return true
}

func (g *Gateway) handleBackends(w http.ResponseWriter, _ *http.Request) {
	type row struct {
		ID       string `json:"id"`
		Addr     string `json:"addr"`
		InFlight int64  `json:"in_flight"`
		Healthy  bool   `json:"healthy"`
	}
	rows := make([]row, 0, len(g.backends))
	for _, b := range g.backends {
		rows = append(rows, row{b.ID, b.Addr, b.InFlight(), b.Healthy()})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"policy":             g.policy.Name(),
		"backends":           rows,
		"retries":            g.retries.Load(),
		"midstream_failures": g.midStreamFailures.Load(),
		"upstream_timeouts":  g.upstreamTimeouts.Load(),
		"stale_conn_retries": g.staleRetries.Load(),
	})
}

// SetAdmission enables admission control (load shedding). Call it before serving.
func (g *Gateway) SetAdmission(cfg admission.Config) { g.admit = admission.New(cfg) }

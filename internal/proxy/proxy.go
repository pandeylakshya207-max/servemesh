package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

const maxBodyBytes = 4 << 20 // 4 MiB

var doneMarker = []byte("[DONE]")

// Gateway routes OpenAI-style chat requests to backends and streams responses back.
type Gateway struct {
	policy   router.Policy
	backends []*router.Backend
	client   *http.Client
	mux      *http.ServeMux
	seq      atomic.Uint64

	// MaxAttempts is how many different backends one request may try before the
	// first response byte reaches the client. Default 3.
	MaxAttempts int

	retries           atomic.Int64
	midStreamFailures atomic.Int64
}

func New(policy router.Policy, backends []*router.Backend) *Gateway {
	g := &Gateway{
		policy:      policy,
		backends:    backends,
		MaxAttempts: 3,
		// No overall Timeout: it would cut off long streams.
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
		mux: http.NewServeMux(),
	}
	g.mux.HandleFunc("POST /v1/chat/completions", g.handleChat)
	g.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	g.mux.HandleFunc("GET /backends", g.handleBackends)
	return g
}

func (g *Gateway) Handler() http.Handler { return g.mux }

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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "request body unreadable or too large", http.StatusBadRequest)
		return
	}
	var cr chatRequest
	if err := json.Unmarshal(body, &cr); err != nil || len(cr.Messages) == 0 {
		http.Error(w, "invalid chat request", http.StatusBadRequest)
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

	tried := make(map[*router.Backend]bool)
	attempts := 0
	var pickErr error
	for attempts < g.maxAttempts() {
		b, perr := g.policy.Pick(req, g.untried(tried))
		if perr != nil {
			pickErr = perr
			break
		}
		tried[b] = true
		attempts++
		if attempts > 1 {
			g.retries.Add(1)
		}
		if g.tryBackend(w, r, b, body, req.ID, attempts) {
			return
		}
	}
	if attempts == 0 {
		if errors.Is(pickErr, router.ErrNoBackends) {
			http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "routing error", http.StatusInternalServerError)
		}
		return
	}
	http.Error(w, "all upstream attempts failed", http.StatusBadGateway)
}

// tryBackend returns true once the response is committed to the client (success,
// a client-side error, or a mid-stream failure) and false if the attempt failed
// before any byte reached the client, so the caller may retry elsewhere.
func (g *Gateway) tryBackend(w http.ResponseWriter, r *http.Request, b *router.Backend, body []byte, reqID string, attempt int) bool {
	b.Acquire()
	defer b.Release()

	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		"http://"+b.Addr+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return true
	}
	up.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(up)
	if err != nil {
		if r.Context().Err() != nil {
			return true // the client went away; not the backend's fault
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
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return true // client went away
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
	})
}

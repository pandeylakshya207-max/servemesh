package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

const maxBodyBytes = 4 << 20 // 4 MiB

// Gateway routes OpenAI-style chat requests to backends and streams responses back.
type Gateway struct {
	policy   router.Policy
	backends []*router.Backend
	client   *http.Client
	mux      *http.ServeMux
	seq      atomic.Uint64
}

func New(policy router.Policy, backends []*router.Backend) *Gateway {
	g := &Gateway{
		policy:   policy,
		backends: backends,
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

	b, err := g.policy.Pick(req, g.backends)
	if errors.Is(err, router.ErrNoBackends) {
		http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "routing error", http.StatusInternalServerError)
		return
	}
	b.Acquire()
	defer b.Release()

	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		"http://"+b.Addr+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	up.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(up)
	if err != nil {
		log.Printf("%s: upstream %s failed: %v", req.ID, b.ID, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Gateway-Policy", g.policy.Name())
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // client went away; the deferred Release runs
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
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
		"policy":   g.policy.Name(),
		"backends": rows,
	})
}

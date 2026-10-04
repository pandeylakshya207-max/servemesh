package backend

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BlockSize is the prefix-cache block size in tokens (matches vLLM's default).
// Only full blocks are cached, so prompts shorter than one block never hit.
const BlockSize = 16

// MockConfig controls the simulated replica's behaviour.
type MockConfig struct {
	ID              string
	PrefillPerToken time.Duration // cost per UNCACHED prompt token
	DecodePerToken  time.Duration // cost per generated token at zero load
	CacheBlocks     int           // prefix-cache capacity in blocks
	Slowdown        float64       // extra decode slowdown per additional in-flight request
}

// Mock is a fake OpenAI-compatible streaming backend with a simulated prefix cache.
type Mock struct {
	cfg MockConfig
	mux *http.ServeMux

	mu    sync.Mutex
	lru   *list.List
	index map[uint64]*list.Element

	inFlight     atomic.Int64
	requests     atomic.Int64
	promptTokens atomic.Int64
	cachedTokens atomic.Int64
}

func NewMock(cfg MockConfig) *Mock {
	if cfg.CacheBlocks <= 0 {
		cfg.CacheBlocks = 4096
	}
	m := &Mock{
		cfg:   cfg,
		mux:   http.NewServeMux(),
		lru:   list.New(),
		index: make(map[uint64]*list.Element),
	}
	m.mux.HandleFunc("POST /v1/chat/completions", m.handleChat)
	m.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	m.mux.HandleFunc("GET /stats", m.handleStats)
	return m
}

func (m *Mock) Handler() http.Handler { return m.mux }

type chatRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	Messages  []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// blockHashes returns one chained hash per full block. Each hash covers the
// block's tokens AND all previous blocks, so equal hashes mean equal prefixes.
func blockHashes(tokens []string) []uint64 {
	var out []uint64
	var prev uint64
	for i := 0; i+BlockSize <= len(tokens); i += BlockSize {
		h := fnv.New64a()
		var buf [8]byte
		for j := 0; j < 8; j++ {
			buf[j] = byte(prev >> (8 * j))
		}
		_, _ = h.Write(buf[:])
		for _, t := range tokens[i : i+BlockSize] {
			_, _ = h.Write([]byte(t))
			_, _ = h.Write([]byte{0})
		}
		prev = h.Sum64()
		out = append(out, prev)
	}
	return out
}

// lookupAndInsert returns how many leading blocks were already cached, then
// inserts/refreshes all of the prompt's blocks (evicting the least recently used).
func (m *Mock) lookupAndInsert(hashes []uint64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	hit := 0
	for _, h := range hashes {
		if _, ok := m.index[h]; !ok {
			break
		}
		hit++
	}
	for _, h := range hashes {
		if el, ok := m.index[h]; ok {
			m.lru.MoveToFront(el)
			continue
		}
		m.index[h] = m.lru.PushFront(h)
		for m.lru.Len() > m.cfg.CacheBlocks {
			back := m.lru.Back()
			m.lru.Remove(back)
			delete(m.index, back.Value.(uint64))
		}
	}
	return hit
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func chunk(id, model, content string, finish bool) []byte {
	choice := map[string]any{"index": 0, "delta": map[string]string{}, "finish_reason": nil}
	if finish {
		choice["finish_reason"] = "stop"
	} else {
		choice["delta"] = map[string]string{"content": content}
	}
	b, _ := json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"model":   model,
		"choices": []any{choice},
	})
	return b
}

func (m *Mock) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 32
	}

	var sb strings.Builder
	for _, msg := range req.Messages {
		sb.WriteString(msg.Role)
		sb.WriteString(": ")
		sb.WriteString(msg.Content)
		sb.WriteString("\n")
	}
	tokens := strings.Fields(sb.String())
	cached := m.lookupAndInsert(blockHashes(tokens)) * BlockSize
	uncached := len(tokens) - cached

	m.inFlight.Add(1)
	defer m.inFlight.Add(-1)
	reqNum := m.requests.Add(1)
	m.promptTokens.Add(int64(len(tokens)))
	m.cachedTokens.Add(int64(cached))

	id := fmt.Sprintf("chatcmpl-%s-%d", m.cfg.ID, reqNum)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Backend-ID", m.cfg.ID)
	w.Header().Set("X-Prompt-Tokens", fmt.Sprint(len(tokens)))
	w.Header().Set("X-Cached-Tokens", fmt.Sprint(cached))
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()

	load := func() float64 { return 1 + m.cfg.Slowdown*float64(m.inFlight.Load()-1) }

	// Prefill: cost only for the tokens that were not in the prefix cache.
	prefill := time.Duration(float64(uncached) * float64(m.cfg.PrefillPerToken) * load())
	if !sleepCtx(r.Context(), prefill) {
		return
	}
	// Decode: one token at a time, slowing down as concurrency rises.
	for i := 0; i < maxTokens; i++ {
		if !sleepCtx(r.Context(), time.Duration(float64(m.cfg.DecodePerToken)*load())) {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", chunk(id, req.Model, "tok ", false))
		flush()
	}
	fmt.Fprintf(w, "data: %s\n\n", chunk(id, req.Model, "", true))
	fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

func (m *Mock) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":            m.cfg.ID,
		"requests":      m.requests.Load(),
		"in_flight":     m.inFlight.Load(),
		"prompt_tokens": m.promptTokens.Load(),
		"cached_tokens": m.cachedTokens.Load(),
	})
}

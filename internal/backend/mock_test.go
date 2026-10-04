package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(cacheBlocks int) *httptest.Server {
	m := NewMock(MockConfig{ID: "t", CacheBlocks: cacheBlocks})
	return httptest.NewServer(m.Handler())
}

func words(tag string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("%s%d", tag, i)
	}
	return strings.Join(parts, " ")
}

func post(t *testing.T, url, prompt string, maxTokens int) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":      "mock",
		"stream":     true,
		"max_tokens": maxTokens,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestStreamsTokensAndDone(t *testing.T) {
	srv := newTestServer(100)
	defer srv.Close()
	_, body := post(t, srv.URL, "hello world", 5)
	if got := strings.Count(body, `"content":`); got != 5 {
		t.Fatalf("got %d content chunks, want 5", got)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatal("stream did not end with [DONE]")
	}
}

func TestRepeatedPromptHitsCache(t *testing.T) {
	srv := newTestServer(100)
	defer srv.Close()
	prompt := words("w", 40) // "user:" + 40 words = 41 tokens = 2 full blocks

	r1, _ := post(t, srv.URL, prompt, 1)
	if got := r1.Header.Get("X-Cached-Tokens"); got != "0" {
		t.Fatalf("first request cached=%s, want 0", got)
	}
	r2, _ := post(t, srv.URL, prompt, 1)
	if got := r2.Header.Get("X-Cached-Tokens"); got != "32" {
		t.Fatalf("second request cached=%s, want 32", got)
	}
}

func TestSharedPrefixPartialHit(t *testing.T) {
	srv := newTestServer(100)
	defer srv.Close()
	common := words("c", 47) // "user:" + 47 = exactly 3 blocks of 16
	post(t, srv.URL, common+" "+words("a", 20), 1)
	r, _ := post(t, srv.URL, common+" "+words("b", 20), 1)
	if got := r.Header.Get("X-Cached-Tokens"); got != "48" {
		t.Fatalf("shared-prefix cached=%s, want 48", got)
	}
	r, _ = post(t, srv.URL, words("z", 40), 1)
	if got := r.Header.Get("X-Cached-Tokens"); got != "0" {
		t.Fatalf("unrelated prompt cached=%s, want 0", got)
	}
}

func TestLRUEviction(t *testing.T) {
	srv := newTestServer(2) // room for exactly one 2-block prompt
	defer srv.Close()
	p1, p2 := words("p", 32), words("q", 32)
	post(t, srv.URL, p1, 1)
	post(t, srv.URL, p2, 1) // evicts p1
	r, _ := post(t, srv.URL, p1, 1)
	if got := r.Header.Get("X-Cached-Tokens"); got != "0" {
		t.Fatalf("evicted prompt cached=%s, want 0", got)
	}
}

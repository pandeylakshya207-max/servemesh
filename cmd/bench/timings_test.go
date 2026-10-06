package main

import (
	"net/http"
	"testing"
)

func TestJSONInt(t *testing.T) {
	line := `data: {"timings":{"cache_n":316,"prompt_n":6,"prompt_ms":47.1}}`
	if n, ok := jsonInt(line, "cache_n"); !ok || n != 316 {
		t.Fatalf("cache_n = %d, %v", n, ok)
	}
	if n, ok := jsonInt(line, "prompt_n"); !ok || n != 6 {
		t.Fatalf("prompt_n = %d, %v", n, ok)
	}
	if _, ok := jsonInt(line, "missing"); ok {
		t.Fatal("found a key that is not there")
	}
}

func TestDoRequestReadsLlamaTimings(t *testing.T) {
	timings := `data: {"choices":[{"finish_reason":"length","index":0,"delta":{}}],"timings":{"cache_n":316,"prompt_n":6,"prompt_ms":47.1}}`
	r := doRequest(http.DefaultClient, serveSSE(t, tokLine, timings, "data: [DONE]"), []byte(`{}`))
	if !r.OK || r.Cached != 316 || r.Prompt != 322 {
		t.Fatalf("OK=%v cached=%d prompt=%d err=%q", r.OK, r.Cached, r.Prompt, r.Err)
	}
}

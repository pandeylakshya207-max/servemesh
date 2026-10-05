package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

const tokLine = `data: {"choices":[{"delta":{"content":"tok "}}]}`

func serveSSE(t *testing.T, lines ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, l := range lines {
			fmt.Fprint(w, l+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDoRequestCompleteStreamIsOK(t *testing.T) {
	r := doRequest(http.DefaultClient, serveSSE(t, tokLine, tokLine, "data: [DONE]"), []byte(`{}`))
	if !r.OK || r.Tokens != 2 {
		t.Fatalf("OK=%v tokens=%d err=%q", r.OK, r.Tokens, r.Err)
	}
}

func TestDoRequestErrorEventIsFailure(t *testing.T) {
	r := doRequest(http.DefaultClient, serveSSE(t, tokLine, `data: {"error":{"message":"x"}}`), []byte(`{}`))
	if r.OK || r.Err != "stream error event" {
		t.Fatalf("OK=%v err=%q", r.OK, r.Err)
	}
}

func TestDoRequestMissingDoneIsFailure(t *testing.T) {
	r := doRequest(http.DefaultClient, serveSSE(t, tokLine), []byte(`{}`))
	if r.OK || r.Err != "incomplete stream" {
		t.Fatalf("OK=%v err=%q", r.OK, r.Err)
	}
}

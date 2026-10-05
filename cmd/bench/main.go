package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type workload interface {
	Name() string
	Next(rng *rand.Rand) []message
}

func words(rng *rand.Rand, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString("w")
		sb.WriteString(strconv.Itoa(rng.Intn(1_000_000)))
	}
	return sb.String()
}

// randomWL: every prompt is unique. No prefix sharing, so caching cannot help.
type randomWL struct{ promptWords int }

func (w *randomWL) Name() string { return "random" }
func (w *randomWL) Next(rng *rand.Rand) []message {
	return []message{{"user", words(rng, w.promptWords)}}
}

// sharedWL: a fixed set of long system prompts; each request picks one and adds a unique question.
type sharedWL struct {
	systems   []string
	userWords int
}

func newShared(rng *rand.Rand, groups, systemWords, userWords int) *sharedWL {
	w := &sharedWL{userWords: userWords}
	for i := 0; i < groups; i++ {
		w.systems = append(w.systems, words(rng, systemWords))
	}
	return w
}

func (w *sharedWL) Name() string { return "shared-system" }
func (w *sharedWL) Next(rng *rand.Rand) []message {
	sys := w.systems[rng.Intn(len(w.systems))]
	return []message{{"system", sys}, {"user", words(rng, w.userWords)}}
}

// multiTurnWL: conversations whose history grows; each request sends the full history.
type multiTurnWL struct {
	convs      [][]message
	turnWords  int
	replyWords int
}

func newMultiTurn(rng *rand.Rand, convs, systemWords, turnWords, replyWords int) *multiTurnWL {
	w := &multiTurnWL{turnWords: turnWords, replyWords: replyWords}
	for i := 0; i < convs; i++ {
		w.convs = append(w.convs, []message{{"system", words(rng, systemWords)}})
	}
	return w
}

func (w *multiTurnWL) Name() string { return "multiturn" }
func (w *multiTurnWL) Next(rng *rand.Rand) []message {
	i := rng.Intn(len(w.convs))
	w.convs[i] = append(w.convs[i], message{"user", words(rng, w.turnWords)})
	req := append([]message(nil), w.convs[i]...)
	w.convs[i] = append(w.convs[i], message{"assistant", words(rng, w.replyWords)})
	return req
}

type result struct {
	Start  time.Duration
	TTFT   time.Duration
	E2E    time.Duration
	OK     bool
	Tokens int
	Prompt int
	Cached int
	Err    string
}

func doRequest(client *http.Client, url string, body []byte) result {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	t0 := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return result{Err: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return result{Err: "request: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return result{Err: "status " + strconv.Itoa(resp.StatusCode)}
	}
	var r result
	r.Prompt, _ = strconv.Atoi(resp.Header.Get("X-Prompt-Tokens"))
	r.Cached, _ = strconv.Atoi(resp.Header.Get("X-Cached-Tokens"))
	sawDone, streamErr := false, false
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, rerr := br.ReadString('\n')
		switch {
		case strings.HasPrefix(line, "data: [DONE]"):
			sawDone = true
		case strings.HasPrefix(line, `data: {"error"`):
			streamErr = true
		case strings.HasPrefix(line, "data: {") && strings.Contains(line, `"content":"`) && !strings.Contains(line, `"content":""`):
			if r.Tokens == 0 {
				r.TTFT = time.Since(t0)
			}
			r.Tokens++
		}
		if rerr != nil {
			if rerr != io.EOF {
				r.Err = "read: " + rerr.Error()
				return r
			}
			break
		}
	}
	r.E2E = time.Since(t0)
	switch {
	case streamErr:
		r.Err = "stream error event"
	case !sawDone:
		r.Err = "incomplete stream"
	case r.Tokens == 0:
		r.Err = "empty stream"
	default:
		r.OK = true
	}
	return r
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

type pcts struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
}

func pctsOf(ds []time.Duration) pcts {
	ms := make([]float64, len(ds))
	for i, d := range ds {
		ms[i] = float64(d) / float64(time.Millisecond)
	}
	sort.Float64s(ms)
	return pcts{percentile(ms, 0.50), percentile(ms, 0.95), percentile(ms, 0.99)}
}

type summary struct {
	Label        string  `json:"label"`
	Workload     string  `json:"workload"`
	RPS          float64 `json:"offered_rps"`
	WindowSec    float64 `json:"window_sec"`
	Seed         int64   `json:"seed"`
	OK           int     `json:"ok"`
	Errors       int     `json:"errors"`
	TTFT         pcts    `json:"ttft"`
	E2E          pcts    `json:"e2e"`
	ReqPerSec    float64 `json:"completed_req_per_sec"`
	TokPerSec    float64 `json:"output_tok_per_sec"`
	CacheHitRate float64 `json:"cache_hit_rate"` // -1 when the backend does not report it
}

func main() {
	url := flag.String("url", "http://localhost:8080", "gateway base URL")
	wlName := flag.String("workload", "shared-system", "random | shared-system | multiturn")
	rps := flag.Float64("rps", 10, "offered load, requests per second (Poisson arrivals)")
	duration := flag.Duration("duration", 30*time.Second, "total run time")
	warmup := flag.Duration("warmup", 5*time.Second, "initial period excluded from statistics")
	maxTokens := flag.Int("max-tokens", 32, "max_tokens per request")
	seed := flag.Int64("seed", 1, "random seed")
	groups := flag.Int("groups", 16, "shared-system: number of distinct system prompts")
	systemWords := flag.Int("system-words", 400, "words in each system prompt")
	userWords := flag.Int("user-words", 30, "words in each unique user message")
	promptWords := flag.Int("prompt-words", 200, "random: words per prompt")
	convs := flag.Int("convs", 50, "multiturn: number of conversations")
	label := flag.String("label", "", "label stored in the results file (e.g. the policy name)")
	out := flag.String("out", "", "write JSON summary to this path")
	flag.Parse()

	if *rps <= 0 {
		fmt.Fprintln(os.Stderr, "rps must be > 0")
		os.Exit(2)
	}
	if *warmup >= *duration {
		*warmup = 0
	}
	rng := rand.New(rand.NewSource(*seed))
	var wl workload
	switch *wlName {
	case "random":
		wl = &randomWL{promptWords: *promptWords}
	case "shared-system":
		wl = newShared(rng, *groups, *systemWords, *userWords)
	case "multiturn":
		wl = newMultiTurn(rng, *convs, *systemWords/4, *userWords, *userWords)
	default:
		fmt.Fprintf(os.Stderr, "unknown workload %q\n", *wlName)
		os.Exit(2)
	}

	client := &http.Client{Transport: &http.Transport{
		MaxIdleConnsPerHost: 1024,
		IdleConnTimeout:     90 * time.Second,
	}}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []result
	)
	start := time.Now()
	deadline := start.Add(*duration)
	nextAt := start
	for {
		nextAt = nextAt.Add(time.Duration(rng.ExpFloat64() / *rps * float64(time.Second)))
		if nextAt.After(deadline) {
			break
		}
		time.Sleep(time.Until(nextAt))
		body, _ := json.Marshal(map[string]any{
			"model":      "mock",
			"stream":     true,
			"max_tokens": *maxTokens,
			"messages":   wl.Next(rng),
		})
		sentAt := time.Now()
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := doRequest(client, *url, body)
			r.Start = sentAt.Sub(start)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}()
	}
	wg.Wait()

	var ttft, e2e []time.Duration
	var okN, errN, tokens, prompt, cached int
	errKinds := map[string]int{}
	for _, r := range results {
		if r.Start < *warmup {
			continue
		}
		if !r.OK {
			errN++
			errKinds[r.Err]++
			continue
		}
		okN++
		ttft = append(ttft, r.TTFT)
		e2e = append(e2e, r.E2E)
		tokens += r.Tokens
		prompt += r.Prompt
		cached += r.Cached
	}
	window := (*duration - *warmup).Seconds()
	s := summary{
		Label: *label, Workload: wl.Name(), RPS: *rps, WindowSec: window, Seed: *seed,
		OK: okN, Errors: errN, TTFT: pctsOf(ttft), E2E: pctsOf(e2e),
		ReqPerSec: float64(okN) / window, TokPerSec: float64(tokens) / window,
		CacheHitRate: -1,
	}
	if prompt > 0 {
		s.CacheHitRate = float64(cached) / float64(prompt)
	}

	fmt.Printf("label=%q workload=%s offered=%.1f rps window=%.0fs seed=%d\n", s.Label, s.Workload, s.RPS, s.WindowSec, s.Seed)
	fmt.Printf("requests: %d ok, %d errors %v\n", s.OK, s.Errors, errKinds)
	fmt.Printf("TTFT  ms: p50=%.0f  p95=%.0f  p99=%.0f\n", s.TTFT.P50, s.TTFT.P95, s.TTFT.P99)
	fmt.Printf("E2E   ms: p50=%.0f  p95=%.0f  p99=%.0f\n", s.E2E.P50, s.E2E.P95, s.E2E.P99)
	fmt.Printf("throughput: %.1f req/s, %.0f output tok/s\n", s.ReqPerSec, s.TokPerSec)
	if s.CacheHitRate >= 0 {
		fmt.Printf("prefix cache hit rate: %.1f%% of prompt tokens\n", 100*s.CacheHitRate)
	} else {
		fmt.Println("prefix cache hit rate: n/a (backend does not report it)")
	}

	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		b, _ := json.MarshalIndent(s, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("wrote", *out)
	}
}

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
	"time"
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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

// jsonInt extracts the integer that follows `"key":` in a JSON line.
func jsonInt(line, key string) (int, bool) {
	i := strings.Index(line, `"`+key+`":`)
	if i < 0 {
		return 0, false
	}
	rest := line[i+len(key)+3:]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:j])
	return n, err == nil
}

type sample struct {
	ttft   time.Duration
	cached int // prompt tokens served from the KV cache
	prefil int // prompt tokens actually processed
}

func one(client *http.Client, url string, body []byte) (sample, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	t0 := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return sample{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return sample{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return sample{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var s sample
	sawDone, gotTok, gotTimings := false, false, false
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, rerr := br.ReadString('\n')
		switch {
		case strings.HasPrefix(line, "data: [DONE]"):
			sawDone = true
		case strings.HasPrefix(line, `data: {"error"`):
			return sample{}, fmt.Errorf("stream error event")
		case strings.HasPrefix(line, "data: {"):
			if strings.Contains(line, `"timings"`) {
				if c, ok := jsonInt(line, "cache_n"); ok {
					s.cached, gotTimings = c, true
				}
				if p, ok := jsonInt(line, "prompt_n"); ok {
					s.prefil = p
				}
			}
			if !gotTok && strings.Contains(line, `"content":"`) && !strings.Contains(line, `"content":""`) {
				gotTok = true
				s.ttft = time.Since(t0)
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				return sample{}, rerr
			}
			break
		}
	}
	switch {
	case !sawDone:
		return sample{}, fmt.Errorf("incomplete stream")
	case !gotTok:
		return sample{}, fmt.Errorf("empty stream")
	case !gotTimings:
		return sample{}, fmt.Errorf("no timings in stream (is the backend llama.cpp?)")
	}
	return s, nil
}

func pct(sorted []float64, p float64) float64 {
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

type result struct {
	Label         string         `json:"label"`
	Seed          int64          `json:"seed"`
	Measured      int            `json:"requests_measured"`
	Errors        map[string]int `json:"errors"`
	HitRate       float64        `json:"cache_hit_rate"`
	MeanPrefilled float64        `json:"mean_prompt_tokens_processed"`
	TTFTp50       float64        `json:"ttft_p50_ms"`
	TTFTp95       float64        `json:"ttft_p95_ms"`
	WarmN         int            `json:"warm_requests"`
	WarmP50       float64        `json:"warm_ttft_p50_ms"`
	ColdN         int            `json:"cold_requests"`
	ColdP50       float64        `json:"cold_ttft_p50_ms"`
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8080", "gateway base URL")
	n := flag.Int("n", 100, "number of sequential requests")
	skip := flag.Int("skip", 12, "ignore the first N requests in the statistics (cold start)")
	groups := flag.Int("groups", 6, "number of distinct system prompts")
	systemWords := flag.Int("system-words", 60, "words per system prompt")
	userWords := flag.Int("user-words", 15, "words in each unique user message")
	maxTokens := flag.Int("max-tokens", 8, "max_tokens per request")
	seed := flag.Int64("seed", 1, "random seed (same seed = same request sequence for every policy)")
	label := flag.String("label", "", "label stored in the results file")
	out := flag.String("out", "", "write a JSON summary to this path")
	flag.Parse()

	rng := rand.New(rand.NewSource(*seed))
	systems := make([]string, *groups)
	for i := range systems {
		systems[i] = words(rng, *systemWords)
	}

	client := &http.Client{}
	var samples []sample
	errs := map[string]int{}
	for i := 0; i < *n; i++ {
		g := rng.Intn(*groups)
		// All random draws happen before the request, so the sequence is identical
		// across policies even if some requests fail.
		body, _ := json.Marshal(map[string]any{
			"model":      "x",
			"stream":     true,
			"max_tokens": *maxTokens,
			"messages":   []message{{"system", systems[g]}, {"user", words(rng, *userWords)}},
		})
		s, err := one(client, *url, body)
		if err != nil {
			errs[err.Error()]++
			continue
		}
		if i >= *skip {
			samples = append(samples, s)
		}
	}

	var all, warm, cold []float64
	var cached, total, prefilled int
	for _, s := range samples {
		ms := float64(s.ttft) / float64(time.Millisecond)
		all = append(all, ms)
		cached += s.cached
		total += s.cached + s.prefil
		prefilled += s.prefil
		if s.cached*2 >= s.cached+s.prefil {
			warm = append(warm, ms)
		} else {
			cold = append(cold, ms)
		}
	}
	sort.Float64s(all)
	sort.Float64s(warm)
	sort.Float64s(cold)

	res := result{Label: *label, Seed: *seed, Measured: len(samples), Errors: errs,
		TTFTp50: pct(all, 0.5), TTFTp95: pct(all, 0.95),
		WarmN: len(warm), WarmP50: pct(warm, 0.5), ColdN: len(cold), ColdP50: pct(cold, 0.5)}
	if total > 0 {
		res.HitRate = float64(cached) / float64(total)
	}
	if len(samples) > 0 {
		res.MeanPrefilled = float64(prefilled) / float64(len(samples))
	}

	fmt.Printf("label=%q seed=%d sequential requests=%d (first %d excluded)\n", res.Label, res.Seed, *n, *skip)
	fmt.Printf("measured: %d ok, errors %v\n", res.Measured, res.Errors)
	fmt.Printf("prefix cache hit rate: %.1f%% of prompt tokens\n", 100*res.HitRate)
	fmt.Printf("mean prompt tokens prefilled per request: %.0f\n", res.MeanPrefilled)
	fmt.Printf("TTFT ms: p50=%.0f p95=%.0f\n", res.TTFTp50, res.TTFTp95)
	fmt.Printf("warm requests (cache covers at least half the prompt): %d, TTFT p50=%.0f ms\n", res.WarmN, res.WarmP50)
	fmt.Printf("cold requests: %d, TTFT p50=%.0f ms\n", res.ColdN, res.ColdP50)

	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("wrote", *out)
	}
}

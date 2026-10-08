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

type sample struct {
	class  string
	start  time.Duration
	ttft   time.Duration
	e2e    time.Duration
	status string // ok | shed | error
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

func send(client *http.Client, url, prio string, body []byte) (string, time.Duration, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t0 := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "error", 0, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Priority", prio)
	resp, err := client.Do(req)
	if err != nil {
		return "error", 0, 0
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		_, _ = io.Copy(io.Discard, resp.Body)
		return "shed", 0, time.Since(t0)
	case resp.StatusCode != http.StatusOK:
		_, _ = io.Copy(io.Discard, resp.Body)
		return "error", 0, 0
	}
	var ttft time.Duration
	sawDone, bad := false, false
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, rerr := br.ReadString('\n')
		switch {
		case strings.HasPrefix(line, "data: [DONE]"):
			sawDone = true
		case strings.HasPrefix(line, `data: {"error"`):
			bad = true
		case strings.HasPrefix(line, "data: {") && strings.Contains(line, `"content":"`) && !strings.Contains(line, `"content":""`):
			if ttft == 0 {
				ttft = time.Since(t0)
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				return "error", 0, 0
			}
			break
		}
	}
	if bad || !sawDone || ttft == 0 {
		return "error", 0, 0
	}
	return "ok", ttft, time.Since(t0)
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

func msSorted(ds []time.Duration) []float64 {
	out := make([]float64, len(ds))
	for i, d := range ds {
		out[i] = float64(d) / float64(time.Millisecond)
	}
	sort.Float64s(out)
	return out
}

type classStats struct {
	Sent          int     `json:"sent"`
	OK            int     `json:"ok"`
	Shed          int     `json:"shed"`
	Errors        int     `json:"errors"`
	GoodputPerSec float64 `json:"goodput_per_sec"`
	TTFTp50       float64 `json:"ttft_p50_ms"`
	TTFTp95       float64 `json:"ttft_p95_ms"`
	TTFTp99       float64 `json:"ttft_p99_ms"`
	E2Ep50        float64 `json:"e2e_p50_ms"`
	E2Ep95        float64 `json:"e2e_p95_ms"`
}

func summarize(samples []sample, class string, windowSec float64) classStats {
	var st classStats
	var ttft, e2e []time.Duration
	for _, s := range samples {
		if s.class != class {
			continue
		}
		st.Sent++
		switch s.status {
		case "ok":
			st.OK++
			ttft = append(ttft, s.ttft)
			e2e = append(e2e, s.e2e)
		case "shed":
			st.Shed++
		default:
			st.Errors++
		}
	}
	t, e := msSorted(ttft), msSorted(e2e)
	st.TTFTp50, st.TTFTp95, st.TTFTp99 = pct(t, 0.50), pct(t, 0.95), pct(t, 0.99)
	st.E2Ep50, st.E2Ep95 = pct(e, 0.50), pct(e, 0.95)
	if windowSec > 0 {
		st.GoodputPerSec = float64(st.OK) / windowSec
	}
	return st
}

type result struct {
	Label     string     `json:"label"`
	RPS       float64    `json:"offered_rps"`
	HighFrac  float64    `json:"high_fraction"`
	Seed      int64      `json:"seed"`
	WindowSec float64    `json:"window_sec"`
	High      classStats `json:"high"`
	Low       classStats `json:"low"`
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8080", "gateway base URL")
	rps := flag.Float64("rps", 60, "offered load, requests per second (Poisson arrivals)")
	duration := flag.Duration("duration", 40*time.Second, "total run time")
	warmup := flag.Duration("warmup", 10*time.Second, "initial period excluded from statistics")
	highFrac := flag.Float64("high-frac", 0.2, "fraction of requests sent with X-Priority: high (the rest are low)")
	promptWords := flag.Int("prompt-words", 50, "words per unique prompt")
	maxTokens := flag.Int("max-tokens", 32, "max_tokens per request")
	seed := flag.Int64("seed", 1, "random seed")
	label := flag.String("label", "", "label stored in the results")
	sendPriority := flag.String("send-priority", "", "if set, send this X-Priority on every request while still tracking classes separately (a cap without priorities)")
	slo := flag.Duration("slo-ttft", time.Second, "first-token latency target for the 'served within target' lines")
	out := flag.String("out", "", "write a JSON summary to this path")
	flag.Parse()

	if *rps <= 0 {
		fmt.Fprintln(os.Stderr, "rps must be > 0")
		os.Exit(2)
	}
	if *warmup >= *duration {
		*warmup = 0
	}
	rng := rand.New(rand.NewSource(*seed))
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1024, IdleConnTimeout: 4 * time.Second}}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		samples []sample
	)
	start := time.Now()
	deadline := start.Add(*duration)
	next := start
	for {
		next = next.Add(time.Duration(rng.ExpFloat64() / *rps * float64(time.Second)))
		if next.After(deadline) {
			break
		}
		time.Sleep(time.Until(next))
		class := "low"
		if rng.Float64() < *highFrac {
			class = "high"
		}
		body, _ := json.Marshal(map[string]any{
			"model":      "mock",
			"stream":     true,
			"max_tokens": *maxTokens,
			"messages":   []map[string]string{{"role": "user", "content": words(rng, *promptWords)}},
		})
		sentAt := time.Now()
		wg.Add(1)
		go func() {
			defer wg.Done()
			prio := class
			if *sendPriority != "" {
				prio = *sendPriority
			}
			status, ttft, e2e := send(client, *url, prio, body)
			mu.Lock()
			samples = append(samples, sample{class: class, start: sentAt.Sub(start), ttft: ttft, e2e: e2e, status: status})
			mu.Unlock()
		}()
	}
	wg.Wait()

	var measured []sample
	for _, s := range samples {
		if s.start >= *warmup {
			measured = append(measured, s)
		}
	}
	window := (*duration - *warmup).Seconds()
	res := result{Label: *label, RPS: *rps, HighFrac: *highFrac, Seed: *seed, WindowSec: window,
		High: summarize(measured, "high", window), Low: summarize(measured, "low", window)}

	fmt.Printf("label=%q offered=%.0f rps, %.0f%% high, window=%.0fs, seed=%d\n", res.Label, res.RPS, 100*res.HighFrac, res.WindowSec, res.Seed)
	fmt.Printf("%-5s %5s %5s %5s %4s %9s   %-26s %-18s\n", "class", "sent", "ok", "shed", "err", "goodput/s", "TTFT ms p50 / p95 / p99", "E2E ms p50 / p95")
	for _, row := range []struct {
		name string
		st   classStats
	}{{"high", res.High}, {"low", res.Low}} {
		fmt.Printf("%-5s %5d %5d %5d %4d %9.1f   %7.0f / %6.0f / %6.0f   %7.0f / %6.0f\n",
			row.name, row.st.Sent, row.st.OK, row.st.Shed, row.st.Errors, row.st.GoodputPerSec,
			row.st.TTFTp50, row.st.TTFTp95, row.st.TTFTp99, row.st.E2Ep50, row.st.E2Ep95)
	}

	for _, c := range []string{"high", "low"} {
		good, total := withinSLO(measured, c, *slo)
		fmt.Printf("%s: %d of %d requests got a first token within %v (%.1f%%)\n", c, good, total, *slo, 100*float64(good)/math.Max(1, float64(total)))
	}

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

// withinSLO counts requests of a class that were served with a first token within slo.
func withinSLO(samples []sample, class string, slo time.Duration) (good, total int) {
	for _, s := range samples {
		if s.class != class {
			continue
		}
		total++
		if s.status == "ok" && s.ttft <= slo {
			good++
		}
	}
	return good, total
}

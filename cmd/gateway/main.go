package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/admission"
	"github.com/pandeylakshya207-max/servemesh/internal/proxy"
	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

func parseBackends(s string) ([]*router.Backend, error) {
	var out []*router.Backend
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, addr, ok := strings.Cut(part, "=")
		if !ok || id == "" || addr == "" {
			return nil, fmt.Errorf("bad backend %q (want id=host:port)", part)
		}
		out = append(out, router.NewBackend(id, addr))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no backends given")
	}
	return out, nil
}

func newPolicy(name string, cacheBlocks int, loadFactor float64, cold string) (router.Policy, error) {
	switch name {
	case "round-robin":
		return router.NewRoundRobin(), nil
	case "least-loaded":
		return router.NewLeastLoaded(), nil
	case "prefix-aware":
		if cold != router.ColdRendezvous && cold != router.ColdLeastLoaded {
			return nil, fmt.Errorf("unknown cold fallback %q (want rendezvous or least-loaded)", cold)
		}
		p := router.NewPrefixAware(cacheBlocks, loadFactor)
		p.ColdFallback = cold
		return p, nil
	}
	return nil, fmt.Errorf("unknown policy %q (want round-robin, least-loaded or prefix-aware)", name)
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	policyName := flag.String("policy", "round-robin", "routing policy: round-robin | least-loaded | prefix-aware")
	backendsFlag := flag.String("backends", "", "comma-separated id=host:port list")
	cacheBlocks := flag.Int("prefix-cache-blocks", 4096, "prefix-aware: assumed per-backend cache size in blocks (match the backends)")
	loadFactor := flag.Float64("load-factor", 1.25, "prefix-aware: max in-flight as a multiple of the average")
	cold := flag.String("cold-fallback", "rendezvous", "prefix-aware: placement when no backend caches the prompt: rendezvous | least-loaded")
	healthInterval := flag.Duration("health-interval", time.Second, "active health-check interval (0 disables)")
	healthTimeout := flag.Duration("health-timeout", 500*time.Millisecond, "per-probe timeout")
	healthPath := flag.String("health-path", "/healthz", "health endpoint path on the backends (llama-server uses /health)")
	maxAttempts := flag.Int("max-attempts", 3, "max backends tried per request before the first response byte")
	headerTimeout := flag.Duration("header-timeout", 30*time.Second, "how long to wait for a backend to start responding (raise for CPU-bound or queueing backends)")
	maxInflight := flag.Int("max-inflight", 0, "admission control: max concurrent requests across all backends (0 = off)")
	maxQueue := flag.Int("max-queue", 64, "admission control: max requests waiting for a slot")
	queueWait := flag.Duration("queue-wait", 2*time.Second, "admission control: wait budget for normal priority (high gets 2x, low gets 0.5x)")
	flag.Parse()

	backends, err := parseBackends(*backendsFlag)
	if err != nil {
		log.Fatal(err)
	}
	policy, err := newPolicy(*policyName, *cacheBlocks, *loadFactor, *cold)
	if err != nil {
		log.Fatal(err)
	}

	gw := proxy.New(policy, backends)
	gw.MaxAttempts = *maxAttempts
	gw.SetResponseHeaderTimeout(*headerTimeout)
	if *maxInflight > 0 {
		gw.SetAdmission(admission.Config{
			MaxInFlight: *maxInflight,
			MaxQueue:    *maxQueue,
			MaxWait:     [3]time.Duration{2 * *queueWait, *queueWait, *queueWait / 2},
		})
		log.Printf("admission control on: max in-flight %d, max queue %d, wait budget %v (normal)", *maxInflight, *maxQueue, *queueWait)
	}
	if *healthInterval > 0 {
		hc := proxy.NewHealthChecker(backends, *healthInterval, *healthTimeout)
		hc.Path = *healthPath
		go hc.Run(context.Background())
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would kill long-lived streaming responses.
	}
	log.Printf("gateway on %s, policy=%s, %d backends", *addr, policy.Name(), len(backends))
	log.Fatal(srv.ListenAndServe())
}

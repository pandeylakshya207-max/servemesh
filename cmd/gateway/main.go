package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

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

func newPolicy(name string, cacheBlocks int, loadFactor float64) (router.Policy, error) {
	switch name {
	case "round-robin":
		return router.NewRoundRobin(), nil
	case "least-loaded":
		return router.NewLeastLoaded(), nil
	case "prefix-aware":
		return router.NewPrefixAware(cacheBlocks, loadFactor), nil
	}
	return nil, fmt.Errorf("unknown policy %q (want round-robin, least-loaded or prefix-aware)", name)
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	policyName := flag.String("policy", "round-robin", "routing policy: round-robin | least-loaded | prefix-aware")
	backendsFlag := flag.String("backends", "", "comma-separated id=host:port list")
	cacheBlocks := flag.Int("prefix-cache-blocks", 4096, "prefix-aware: assumed per-backend cache size in blocks (match the backends)")
	loadFactor := flag.Float64("load-factor", 1.25, "prefix-aware: max in-flight as a multiple of the average")
	flag.Parse()

	backends, err := parseBackends(*backendsFlag)
	if err != nil {
		log.Fatal(err)
	}
	policy, err := newPolicy(*policyName, *cacheBlocks, *loadFactor)
	if err != nil {
		log.Fatal(err)
	}

	gw := proxy.New(policy, backends)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would kill long-lived streaming responses.
	}
	log.Printf("gateway on %s, policy=%s, %d backends", *addr, policy.Name(), len(backends))
	log.Fatal(srv.ListenAndServe())
}

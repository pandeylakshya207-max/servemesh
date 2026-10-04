package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/pandeylakshya207-max/servemesh/internal/backend"
)

func main() {
	id := flag.String("id", "mock-0", "backend id")
	addr := flag.String("addr", ":9000", "listen address")
	prefill := flag.Duration("prefill-per-token", 2*time.Millisecond, "prefill cost per uncached prompt token")
	decode := flag.Duration("decode-per-token", 10*time.Millisecond, "decode cost per generated token at zero load")
	cacheBlocks := flag.Int("cache-blocks", 4096, "prefix cache capacity in 16-token blocks")
	slowdown := flag.Float64("slowdown", 0.1, "decode slowdown per additional in-flight request")
	flag.Parse()

	m := backend.NewMock(backend.MockConfig{
		ID:              *id,
		PrefillPerToken: *prefill,
		DecodePerToken:  *decode,
		CacheBlocks:     *cacheBlocks,
		Slowdown:        *slowdown,
	})
	log.Printf("mock backend %s listening on %s", *id, *addr)
	log.Fatal(http.ListenAndServe(*addr, m.Handler()))
}

package backend

import (
	"fmt"
	"testing"

	"github.com/pandeylakshya207-max/servemesh/internal/router"
)

func TestBlockHashesMatchRouter(t *testing.T) {
	if BlockSize != router.PrefixBlockSize {
		t.Fatalf("BlockSize %d != router.PrefixBlockSize %d", BlockSize, router.PrefixBlockSize)
	}
	for n := 0; n < 100; n += 7 {
		tokens := make([]string, n)
		for i := range tokens {
			tokens[i] = fmt.Sprintf("t%d", i*31+n)
		}
		a, b := blockHashes(tokens), router.BlockHashes(tokens)
		if len(a) != len(b) {
			t.Fatalf("n=%d: %d vs %d hashes", n, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("n=%d: hash %d differs", n, i)
			}
		}
	}
}

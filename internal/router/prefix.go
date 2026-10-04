package router

import (
	"container/list"
	"hash/fnv"
	"math"
	"strings"
	"sync"
)

// PrefixBlockSize is the prefix-cache block size in tokens. It must match the backend's.
const PrefixBlockSize = 16

// BlockHashes returns one chained hash per full block of tokens. Each hash covers
// the block's tokens and all earlier blocks, so equal hashes mean equal prefixes.
func BlockHashes(tokens []string) []uint64 {
	var out []uint64
	var prev uint64
	for i := 0; i+PrefixBlockSize <= len(tokens); i += PrefixBlockSize {
		h := fnv.New64a()
		var buf [8]byte
		for j := 0; j < 8; j++ {
			buf[j] = byte(prev >> (8 * j))
		}
		_, _ = h.Write(buf[:])
		for _, t := range tokens[i : i+PrefixBlockSize] {
			_, _ = h.Write([]byte(t))
			_, _ = h.Write([]byte{0})
		}
		prev = h.Sum64()
		out = append(out, prev)
	}
	return out
}

// blockSet is a fixed-capacity LRU set of block hashes: the gateway's model of
// one backend's prefix cache.
type blockSet struct {
	cap int
	lru *list.List
	idx map[uint64]*list.Element
}

func newBlockSet(capacity int) *blockSet {
	return &blockSet{cap: capacity, lru: list.New(), idx: make(map[uint64]*list.Element)}
}

func (s *blockSet) has(h uint64) bool {
	_, ok := s.idx[h]
	return ok
}

func (s *blockSet) touch(h uint64) {
	if el, ok := s.idx[h]; ok {
		s.lru.MoveToFront(el)
		return
	}
	s.idx[h] = s.lru.PushFront(h)
	for s.lru.Len() > s.cap {
		back := s.lru.Back()
		s.lru.Remove(back)
		delete(s.idx, back.Value.(uint64))
	}
}

func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

func hrwScore(key uint64, id string) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for j := 0; j < 8; j++ {
		buf[j] = byte(key >> (8 * j))
	}
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(id))
	return mix64(h.Sum64())
}

// rendezvous picks the candidate with the highest hash score for key
// (highest-random-weight hashing): stable, and spreads keys evenly.
func rendezvous(key uint64, cands []*Backend) *Backend {
	var best *Backend
	var bestScore uint64
	for _, b := range cands {
		if s := hrwScore(key, b.ID); best == nil || s > bestScore {
			best, bestScore = b, s
		}
	}
	return best
}

// PrefixAware routes to the backend with the longest cached prefix match,
// subject to a bounded-load cap.
type PrefixAware struct {
	mu         sync.Mutex
	capBlocks  int
	loadFactor float64
	sets       map[string]*blockSet
}

// NewPrefixAware: capBlocks should match each backend's real cache size;
// loadFactor (>= 1) bounds a backend's in-flight count to loadFactor x the average.
func NewPrefixAware(capBlocks int, loadFactor float64) *PrefixAware {
	if capBlocks <= 0 {
		capBlocks = 4096
	}
	if loadFactor < 1 {
		loadFactor = 1.25
	}
	return &PrefixAware{
		capBlocks:  capBlocks,
		loadFactor: loadFactor,
		sets:       make(map[string]*blockSet),
	}
}

func (p *PrefixAware) Name() string { return "prefix-aware" }

func (p *PrefixAware) matched(id string, hashes []uint64) int {
	s := p.sets[id]
	if s == nil {
		return 0
	}
	n := 0
	for _, h := range hashes {
		if !s.has(h) {
			break
		}
		n++
	}
	return n
}

func (p *PrefixAware) record(id string, hashes []uint64) {
	s := p.sets[id]
	if s == nil {
		s = newBlockSet(p.capBlocks)
		p.sets[id] = s
	}
	for _, h := range hashes {
		s.touch(h)
	}
}

func (p *PrefixAware) Pick(req *Request, backends []*Backend) (*Backend, error) {
	h := healthy(backends)
	if len(h) == 0 {
		return nil, ErrNoBackends
	}
	hashes := BlockHashes(strings.Fields(req.Prompt))

	p.mu.Lock()
	defer p.mu.Unlock()

	// Bounded load: exclude backends whose in-flight count is over the cap.
	var total int64
	for _, b := range h {
		total += b.InFlight()
	}
	avg := float64(total+1) / float64(len(h))
	limit := int64(math.Ceil(p.loadFactor * avg))
	if limit < 1 {
		limit = 1
	}
	cands := make([]*Backend, 0, len(h))
	for _, b := range h {
		if b.InFlight() < limit {
			cands = append(cands, b)
		}
	}
	if len(cands) == 0 {
		cands = h
	}

	// Longest matching prefix wins; ties go to the less loaded backend.
	var best *Backend
	bestMatch := -1
	for _, b := range cands {
		m := p.matched(b.ID, hashes)
		if best == nil || m > bestMatch || (m == bestMatch && b.InFlight() < best.InFlight()) {
			best, bestMatch = b, m
		}
	}
	// Nothing cached anywhere: use a stable "home" so a new prefix concentrates.
	if bestMatch == 0 && len(hashes) > 0 {
		best = rendezvous(hashes[0], cands)
	}

	p.record(best.ID, hashes)
	return best, nil
}

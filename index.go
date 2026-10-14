package main

import (
	"slices"
	"sort"
)

// Trigram index
//
// While the store records a unit (see store.go) it splits the unit's text into
// chunks of about chunkSize bytes, ending at line boundaries, and records which
// ASCII-lowercased trigrams (3-byte sequences) occur in each chunk. The (trigram,
// chunk) pairs are buffered up to pairBudget and appended to bucket files (see
// bucket.go). A query that needs some literal text reads only the chunks that
// contain all of its trigrams, then checks the lines exactly.

// Variables so tests can shrink them.
var (
	chunkSize  int64 = 256 << 10
	pairBudget       = 2 << 20 // buffered pairs before writing buckets (16MB)
)

type chunkRec struct {
	unit int32 // index into store.units
	off  int64 // byte offset in the unit's text
	n    int64
	line int // number of the chunk's first line
}

// chunker receives a unit's text and records its chunks and trigrams.
type chunker struct {
	s          *store
	u          *unit
	unit       int32
	start      int64 // offset of the current chunk
	size       int64 // bytes in the current chunk
	line, nl   int   // first line of the current chunk, lines in it
	w0, w1     byte  // previous two bytes
	wn         int
	chunkCount int
}

func (s *store) newChunker(u *unit) *chunker {
	u.id = int32(len(s.units))
	s.units = append(s.units, u)
	if s.bits == nil {
		s.bits = make([]uint64, 1<<24/64)
	}
	u.firstChunk = int32(len(s.chunks))
	return &chunker{s: s, u: u, unit: u.id, line: 1}
}

func (c *chunker) Write(p []byte) (int, error) {
	s := c.s
	for _, b := range p {
		c.size++
		if b == '\n' {
			c.wn = 0
			c.nl++
			if c.size >= chunkSize {
				if err := c.commit(); err != nil {
					return 0, err
				}
			}
			continue
		}
		b = lowerASCII(b)
		if c.wn == 2 {
			t := uint32(c.w0)<<16 | uint32(c.w1)<<8 | uint32(b)
			w, bit := t>>6, uint64(1)<<(t&63)
			if s.bits[w]&bit == 0 {
				s.bits[w] |= bit
				s.setTris = append(s.setTris, t)
			}
		} else {
			c.wn++
		}
		c.w0, c.w1 = c.w1, b
	}
	return len(p), nil
}

// commit ends the current chunk.
func (c *chunker) commit() error {
	s := c.s
	id := uint64(len(s.chunks))
	s.chunks = append(s.chunks, chunkRec{unit: c.unit, off: c.start, n: c.size, line: c.line})
	for _, t := range s.setTris {
		s.pairs = append(s.pairs, uint64(t)<<32|id)
		s.bits[t>>6] = 0
	}
	s.setTris = s.setTris[:0]
	c.start += c.size
	c.line += c.nl
	c.size, c.nl = 0, 0
	c.chunkCount++
	if len(s.pairs) >= pairBudget {
		return s.flushPairs()
	}
	return nil
}

// close ends the last chunk.
func (c *chunker) close() error {
	var err error
	if c.size > 0 {
		err = c.commit()
	}
	c.u.nChunks = int32(c.chunkCount)
	return err
}

// discard drops trigrams collected for an unfinished chunk (on errors).
func (s *store) discardChunk() {
	for _, t := range s.setTris {
		s.bits[t>>6] = 0
	}
	s.setTris = s.setTris[:0]
}

func (s *store) flushPairs() error {
	if len(s.pairs) == 0 {
		return nil
	}
	if err := appendBuckets(s.dir, s.pairs, &s.lens); err != nil {
		return err
	}
	s.pairs = s.pairs[:0]
	return nil
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// trigramsOf returns the distinct lowercased trigrams of the terms. Terms shorter
// than 3 bytes contribute nothing.
func trigramsOf(terms [][]byte) []uint32 {
	var out []uint32
	for _, t := range terms {
		for i := 0; i+3 <= len(t); i++ {
			tri := uint32(lowerASCII(t[i]))<<16 | uint32(lowerASCII(t[i+1]))<<8 | uint32(lowerASCII(t[i+2]))
			if !slices.Contains(out, tri) {
				out = append(out, tri)
			}
		}
	}
	return out
}

// candidates returns, per unit ID, the chunks that contain every trigram of the
// terms. ok is false when the terms have no trigrams or the index can't be read;
// then everything has to be read.
func (sn *storeSnap) candidates(terms [][]byte) (map[int32][]chunkRec, bool) {
	tris := trigramsOf(terms)
	if sn == nil || len(tris) == 0 {
		return nil, false
	}
	found, err := lookupBuckets(sn.dir, &sn.lens, tris)
	if err != nil {
		return nil, false
	}
	lists := make([][]uint32, 0, len(tris))
	for _, t := range tris {
		lists = append(lists, found[t])
	}
	sort.Slice(lists, func(a, b int) bool { return len(lists[a]) < len(lists[b]) })
	cur := lists[0]
	for _, l := range lists[1:] {
		if len(cur) == 0 {
			break
		}
		cur = intersect(cur, l)
	}
	out := map[int32][]chunkRec{}
	for _, c := range cur {
		if int(c) < len(sn.chunks) {
			ch := sn.chunks[c]
			out[ch.unit] = append(out[ch.unit], ch)
		}
	}
	return out, true
}

func intersect(a, b []uint32) []uint32 {
	var out []uint32
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

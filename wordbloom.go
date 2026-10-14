package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
)

// Word filters
//
// Next to the trigram index, every chunk gets a Bloom filter of its words:
// runs of ASCII letters, digits and '_', lowercased (any other byte ends a
// word). Trigrams cannot tell IDs made of few characters apart (every
// 3-character piece of a hex trace ID occurs in almost every chunk); the word
// filter can. A query checks its whole words against the filters of the
// trigram candidates (or of all chunks, for words shorter than a trigram).
// A Bloom filter has false positives (about 1% at 10 bits per word), never
// false negatives, so a chunk is only read in vain, never missed.
//
// The filters are appended to words.bf; chunkRec.bf/bfn locate a chunk's filter.

const (
	bloomFile     = "words.bf"
	bloomBitsWord = 10 // bits per distinct word
	bloomHashes   = 7
)

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c >= 'A' && c <= 'Z'
}

const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

// wordHash hashes a lowercased ASCII word (FNV-1a).
func wordHash(w string) uint64 {
	h := uint64(fnvOffset)
	for i := 0; i < len(w); i++ {
		h = (h ^ uint64(lowerASCII(w[i]))) * fnvPrime
	}
	return h
}

// asciiWords splits s into the words the filters hold.
func asciiWords(s string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		if i < len(s) && isWordByte(s[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, s[start:i])
			start = -1
		}
	}
	return out
}

// bloomPositions calls f with the bit positions of h in a filter of m bits.
func bloomPositions(h uint64, m uint64, f func(p uint64) bool) bool {
	h1, h2 := h, h>>33|h<<31|1
	for i := uint64(0); i < bloomHashes; i++ {
		if !f((h1 + i*h2) % m) {
			return false
		}
	}
	return true
}

// buildBloom returns the filter of a chunk's word hashes as little-endian uint64s.
func buildBloom(words map[uint64]struct{}) []byte {
	n := max(1, (len(words)*bloomBitsWord+63)/64)
	bits := make([]uint64, n)
	m := uint64(n * 64)
	for h := range words {
		bloomPositions(h, m, func(p uint64) bool { bits[p/64] |= 1 << (p % 64); return true })
	}
	out := make([]byte, 8*n)
	for i, b := range bits {
		binary.LittleEndian.PutUint64(out[8*i:], b)
	}
	return out
}

// bloomHas reports whether a filter may hold every word.
func bloomHas(filter []byte, hashes []uint64) bool {
	m := uint64(len(filter) * 8)
	if m == 0 {
		return false
	}
	for _, h := range hashes {
		if !bloomPositions(h, m, func(p uint64) bool {
			return filter[p/8]&(1<<(p%8)) != 0 // little-endian uint64s: bit p is in byte p/8
		}) {
			return false
		}
	}
	return true
}

// wordFilter keeps only the chunks whose filters may hold all words.
func (sn *storeSnap) wordFilter(cand map[int32][]chunkRec, words []string) (map[int32][]chunkRec, error) {
	if len(words) == 0 {
		return cand, nil
	}
	hashes := make([]uint64, len(words))
	for i, w := range words {
		hashes[i] = wordHash(w)
	}
	f, err := os.Open(filepath.Join(sn.dir, bloomFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[int32][]chunkRec{}
	var buf []byte
	for u, chs := range cand {
		for _, ch := range chs {
			buf = append(buf[:0], make([]byte, 8*ch.bfn)...)
			if _, err := f.ReadAt(buf, ch.bf); err != nil {
				return nil, err
			}
			if bloomHas(buf, hashes) {
				out[u] = append(out[u], ch)
			}
		}
	}
	return out, nil
}

// flushBlooms appends the buffered filters to words.bf.
func (s *store) flushBlooms() error {
	if len(s.bfBuf) == 0 {
		return nil
	}
	if err := writeAt(filepath.Join(s.dir, bloomFile), s.bfBuf, s.bfLen); err != nil {
		return err
	}
	s.bfLen += int64(len(s.bfBuf))
	s.bfBuf = s.bfBuf[:0]
	return nil
}

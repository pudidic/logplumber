package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// Bucket files: trigrams are spread over numBuckets files by hash, and the files are only ever appended to.
//
// Entry format: trigram (3 bytes) | chunk count uvarint | list size in bytes uvarint | chunk ID deltas uvarint...
//
// A trigram can have several entries in a file, and later entries always hold larger chunk IDs,
// so reading front to back and concatenating yields a list sorted by chunk ID.
const (
	bucketBits = 8
	numBuckets = 1 << bucketBits
)

func bucketOf(tri uint32) int { return int(tri * 0x9E3779B1 >> (32 - bucketBits)) }

func bucketName(b int) string { return fmt.Sprintf("b%03d.idx", b) }

// appendBuckets appends (trigram<<32 | chunk ID) pairs to the bucket files and advances lens.
// Writes start at the committed length (lens), overwriting any data left by an interrupted write.
func appendBuckets(dir string, pairs []uint64, lens *[numBuckets]int64) error {
	for i, p := range pairs {
		pairs[i] = p | uint64(bucketOf(uint32(p>>32)))<<56
	}
	slices.Sort(pairs)

	var buf, list []byte
	for i := 0; i < len(pairs); {
		b := int(pairs[i] >> 56)
		buf = buf[:0]
		for i < len(pairs) && int(pairs[i]>>56) == b {
			key := pairs[i] >> 32 // bucket + trigram
			list = list[:0]
			var prev uint32
			cnt := 0
			for ; i < len(pairs) && pairs[i]>>32 == key; i++ {
				c := uint32(pairs[i])
				list = binary.AppendUvarint(list, uint64(c-prev))
				prev = c
				cnt++
			}
			tri := uint32(key) & 0xFFFFFF
			buf = append(buf, byte(tri>>16), byte(tri>>8), byte(tri))
			buf = binary.AppendUvarint(buf, uint64(cnt))
			buf = binary.AppendUvarint(buf, uint64(len(list)))
			buf = append(buf, list...)
		}
		if err := writeAt(filepath.Join(dir, bucketName(b)), buf, lens[b]); err != nil {
			return err
		}
		lens[b] += int64(len(buf))
	}
	return nil
}

func writeAt(path string, data []byte, off int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteAt(data, off)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// lookupBuckets returns the chunk ID list (ascending) for each trigram.
// Bucket files are read only up to their committed length, so concurrent appends by the indexer are safe.
func lookupBuckets(dir string, lens *[numBuckets]int64, tris []uint32) (map[uint32][]uint32, error) {
	byBucket := map[int][]uint32{}
	for _, t := range tris {
		b := bucketOf(t)
		byBucket[b] = append(byBucket[b], t)
	}
	out := make(map[uint32][]uint32, len(tris))
	for b, want := range byBucket {
		if lens[b] == 0 {
			continue
		}
		f, err := os.Open(filepath.Join(dir, bucketName(b)))
		if err != nil {
			return nil, err
		}
		err = scanBucket(bufio.NewReaderSize(io.NewSectionReader(f, 0, lens[b]), 64<<10), want, out)
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanBucket(br *bufio.Reader, want []uint32, out map[uint32][]uint32) error {
	var hdr [3]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		tri := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])
		cnt, err := binary.ReadUvarint(br)
		if err != nil {
			return err
		}
		size, err := binary.ReadUvarint(br)
		if err != nil {
			return err
		}
		if !slices.Contains(want, tri) {
			if _, err := br.Discard(int(size)); err != nil {
				return err
			}
			continue
		}
		list := out[tri]
		var prev uint64
		for i := uint64(0); i < cnt; i++ {
			d, err := binary.ReadUvarint(br)
			if err != nil {
				return err
			}
			prev += d
			list = append(list, uint32(prev))
		}
		out[tri] = list
	}
}

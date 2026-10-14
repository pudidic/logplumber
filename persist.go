package main

import (
	"bufio"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Persistent index
//
// An interactive plumb keeps its index in the user cache directory (or
// $PLUMB_INDEX_DIR) across sessions, so only new or changed files are indexed
// again. Files are recorded by absolute path, so every directory searched stays
// indexed. manifest.gob describes the index: the recorded files, their units
// and chunks, and the committed lengths of the bucket and word-filter files. It
// is replaced atomically at the end of every indexing pass, every 30s during a
// long pass, and when plumb exits; data written after the last manifest (by a
// crash) is overwritten, since writes start at the committed lengths.
//
// When the files of a changed or deleted source make up more than half of the
// index, it is rebuilt from scratch; their cache files are deleted at start.
// If another plumb holds the index, a temporary one is used, as with -c.

const (
	manifestName    = "manifest.gob"
	manifestVersion = 1
	saveEvery       = 30 * time.Second
)

// staleMinBytes: an index smaller than this is never rebuilt for being stale
// (a variable so tests can lower it).
var staleMinBytes int64 = 64 << 20

// indexDir is where the persistent index lives.
func indexDir() (string, error) {
	if d := os.Getenv("PLUMB_INDEX_DIR"); d != "" {
		return d, nil
	}
	c, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(c, "plumb", "index"), nil
}

// openIndex opens the persistent index, or a temporary one when it is in use
// or cannot be opened. note explains a fallback.
func openIndex() (s *store, note string, err error) {
	dir, err := indexDir()
	if err == nil {
		s, err = openPersistentStore(dir)
		if err == nil {
			return s, "", nil
		}
		note = fmt.Sprintf("using a temporary index (%s: %v)", dir, err)
	}
	s, err = openStore()
	return s, note, err
}

func openPersistentStore(dir string) (*store, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lock, err := lockFile(filepath.Join(dir, "lock"))
	if err != nil {
		return nil, errLocked
	}
	s := &store{dir: dir, lock: lock, persistent: true, rootCh: make(chan string, 1), done: make(chan struct{}),
		srcs: map[string]*srcEntry{}, pending: map[string]*srcEntry{}}
	if err := s.loadManifest(); err != nil {
		// unreadable, from another version, or mostly stale: start over
		if err := s.wipe(); err != nil {
			lock.Close()
			return nil, err
		}
	}
	s.start()
	return s, nil
}

// start publishes what is recorded and starts the indexing goroutine.
func (s *store) start() {
	n := len(s.chunks)
	s.snap.Store(&storeSnap{srcs: s.srcs, dir: s.dir, lens: s.lens, chunks: s.chunks[:n:n]})
	s.st.files, s.lastSave = len(s.srcs), time.Now()
	st := s.st
	s.status.Store(&st)
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go s.run(ctx)
}

// deleteIndexQuestion is asked when plumb exits; the default keeps the index.
func deleteIndexQuestion(s *store) string {
	st := s.status.Load()
	return fmt.Sprintf("Delete the index (%s files, %s in %s)? Keeping it makes the next start faster. [y/N] ",
		commas(int64(st.files)), human(st.disk+s.indexBytes()), s.dir)
}

// askDeleteIndex asks on exit (line input) whether to delete the index; Enter
// or the end of input keeps it.
func askDeleteIndex(s *store) bool {
	fmt.Print(deleteIndexQuestion(s))
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if line == "" {
		fmt.Println()
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// indexBytes is the size of the bucket and word-filter files. It reads only the
// published snapshot and the file system, so it is safe while indexing runs.
func (s *store) indexBytes() int64 {
	var n int64
	for _, l := range s.snap.Load().lens {
		n += l
	}
	if fi, err := os.Stat(filepath.Join(s.dir, bloomFile)); err == nil {
		n += fi.Size()
	}
	return n
}

// remove stops indexing and deletes the index directory.
func (s *store) remove() error {
	s.cancel()
	<-s.done
	s.lock.Close()
	var err error
	for range 10 { // a query that just finished may still hold a file open (Windows)
		if err = os.RemoveAll(s.dir); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// clear deletes the index and starts indexing root again.
func (s *store) clear(root string) error {
	s.cancel()
	<-s.done
	if err := s.wipe(); err != nil {
		return err
	}
	s.pending, s.bfBuf, s.pairs = map[string]*srcEntry{}, nil, nil
	clear(s.words)
	s.st = storeStatus{}
	s.start()
	s.setRoot(root)
	return nil
}

// wipe deletes everything in the index directory except the lock.
func (s *store) wipe() error {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.Name() != "lock" {
			if err := os.RemoveAll(filepath.Join(s.dir, e.Name())); err != nil {
				return err
			}
		}
	}
	s.srcs, s.units, s.chunks = map[string]*srcEntry{}, nil, nil
	s.lens, s.bfLen, s.nextFile, s.st.disk = [numBuckets]int64{}, 0, 0, 0
	return nil
}

// ---------------- manifest ----------------

type manifestUnit struct {
	Member, Logical, Path string
	Size                  int64
	Binary                bool
	ID, First, N          int32
}

type manifestSrc struct {
	Size, Mtime int64
	Units       []int32        // indexes into Units, in order; -1: the next of Binary
	Binary      []manifestUnit // binary members (not indexed)
}

type manifestChunk struct {
	Unit       int32
	Off, N, BF int64
	Line       int
	BFN        uint32
}

type manifest struct {
	Version  int
	Units    []manifestUnit
	Srcs     map[string]manifestSrc
	Chunks   []manifestChunk
	Lens     [numBuckets]int64
	BFLen    int64
	NextFile int
	Disk     int64
}

// saveManifest writes the manifest of everything published so far. Called by
// the indexing goroutine (or after it stopped).
func (s *store) saveManifest() error {
	if !s.persistent {
		return nil
	}
	m := manifest{Version: manifestVersion, Srcs: make(map[string]manifestSrc, len(s.srcs)),
		Lens: s.lens, BFLen: s.bfLen, NextFile: s.nextFile, Disk: s.st.disk}
	m.Units = make([]manifestUnit, len(s.units))
	for i, u := range s.units {
		m.Units[i] = manifestUnit{Member: u.member, Logical: u.logical, Path: u.path, Size: u.size,
			ID: u.id, First: u.firstChunk, N: u.nChunks}
	}
	m.Chunks = make([]manifestChunk, len(s.chunks))
	for i, c := range s.chunks {
		m.Chunks[i] = manifestChunk{Unit: c.unit, Off: c.off, N: c.n, Line: c.line, BF: c.bf, BFN: c.bfn}
	}
	for p, e := range s.srcs {
		ms := manifestSrc{Size: e.size, Mtime: e.mtime}
		for _, u := range e.units {
			if u.binary || u.id < 0 {
				ms.Units = append(ms.Units, -1)
				ms.Binary = append(ms.Binary, manifestUnit{Member: u.member, Logical: u.logical, Binary: true, ID: -1})
				continue
			}
			ms.Units = append(ms.Units, u.id)
		}
		m.Srcs[p] = ms
	}
	tmp, err := os.CreateTemp(s.dir, "manifest-*.tmp")
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(tmp).Encode(&m); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, manifestName)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	s.lastSave = time.Now()
	return nil
}

var errStaleIndex = errors.New("mostly stale")

// loadManifest restores the index from its manifest. Sources whose files changed
// since stay recorded until the next pass replaces them; cache files nothing
// refers to any more are deleted.
func (s *store) loadManifest() error {
	f, err := os.Open(filepath.Join(s.dir, manifestName))
	if os.IsNotExist(err) {
		return s.wipe() // a new index: also clears leftovers of an interrupted first run
	}
	if err != nil {
		return err
	}
	var m manifest
	err = gob.NewDecoder(f).Decode(&m)
	f.Close()
	if err != nil {
		return err
	}
	if m.Version != manifestVersion {
		return fmt.Errorf("index version %d", m.Version)
	}
	// the data files must hold at least what the manifest committed
	for b, l := range m.Lens {
		if l == 0 {
			continue
		}
		if fi, err := os.Stat(filepath.Join(s.dir, bucketName(b))); err != nil || fi.Size() < l {
			return errors.New("bucket files are shorter than the manifest")
		}
	}
	if fi, err := os.Stat(filepath.Join(s.dir, bloomFile)); m.BFLen > 0 && (err != nil || fi.Size() < m.BFLen) {
		return errors.New("word filters are shorter than the manifest")
	}
	s.units = make([]*unit, len(m.Units))
	for i, mu := range m.Units {
		s.units[i] = &unit{member: mu.Member, logical: mu.Logical, path: mu.Path, size: mu.Size,
			id: mu.ID, firstChunk: mu.First, nChunks: mu.N}
	}
	s.chunks = make([]chunkRec, len(m.Chunks))
	for i, c := range m.Chunks {
		s.chunks[i] = chunkRec{unit: c.Unit, off: c.Off, n: c.N, line: c.Line, bf: c.BF, bfn: c.BFN}
	}
	s.lens, s.bfLen, s.nextFile, s.st.disk = m.Lens, m.BFLen, m.NextFile, m.Disk

	// Keep the sources that are still there and unchanged; the rest is stale.
	live := map[string]bool{} // cache files still in use
	var liveBytes, allBytes int64
	for _, u := range s.units {
		allBytes += u.size
	}
	for p, ms := range m.Srcs {
		fi, err := os.Stat(p)
		e := &srcEntry{size: ms.Size, mtime: ms.Mtime}
		if err != nil || !e.valid(fi) {
			continue
		}
		ok, nb := true, 0
		for _, id := range ms.Units {
			if id < 0 && nb < len(ms.Binary) {
				b := ms.Binary[nb]
				nb++
				e.units = append(e.units, &unit{member: b.Member, logical: b.Logical, binary: true, id: -1})
				continue
			}
			if id < 0 || int(id) >= len(s.units) {
				ok = false
				break
			}
			u := s.units[id]
			if strings.HasPrefix(u.path, s.dir) {
				if _, err := os.Stat(u.path); err != nil {
					ok = false // its cache file is gone
					break
				}
			}
			e.units = append(e.units, u)
		}
		if !ok {
			continue
		}
		s.srcs[p] = e
		for _, u := range e.units {
			if u.id >= 0 {
				liveBytes += u.size
				live[u.path] = true
			}
		}
	}
	if allBytes > staleMinBytes && liveBytes*2 < allBytes {
		return errStaleIndex
	}
	// delete cache files of stale sources
	ents, _ := os.ReadDir(s.dir)
	for _, ent := range ents {
		name := ent.Name()
		var n int
		if _, err := fmt.Sscanf(name, cacheFileName, &n); err == nil && strings.HasSuffix(name, ".txt") {
			p := filepath.Join(s.dir, name)
			if !live[p] {
				if fi, err := ent.Info(); err == nil {
					s.st.disk -= fi.Size()
				}
				os.Remove(p)
			}
		}
	}
	s.st.disk = max(s.st.disk, 0)
	return nil
}

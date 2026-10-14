package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// The store is the temporary on-disk index of an interactive session. It lives in
// a fresh directory under the OS temp directory and is deleted when psh exits; a
// directory left behind by a crashed psh is deleted the next time psh starts (its
// lock file is no longer held).
//
// A background goroutine walks the current directory and records every file. Text
// inside archives and compressed files is written to plain cache files, so queries
// read it without unpacking again. Indexing pauses while a query runs.

const (
	storePrefix   = "psh-index-"
	recheckEvery  = 30 * time.Second
	publishEvery  = time.Second
	copyBufSize   = 256 << 10
	cacheFileName = "u%d.txt"
)

var errLocked = errors.New("in use by another psh")

// unit is one plain text source: a file on disk or a member of an archive.
type unit struct {
	member              string // "::path" suffix of an archive member, "" for the file itself
	logical             string // base name used by name filters
	path                string // plain text to read: the original file or a decompressed copy
	size                int64  // bytes of text at path
	binary              bool
	id                  int32 // index into store.units; its chunks are in the trigram index
	firstChunk, nChunks int32 // the unit's chunks: store.chunks[firstChunk:firstChunk+nChunks]
}

// srcEntry records a file on disk as it was when indexed.
type srcEntry struct {
	size, mtime int64
	units       []*unit
}

func (e *srcEntry) valid(fi os.FileInfo) bool {
	return fi.Mode().IsRegular() && fi.Size() == e.size && fi.ModTime().UnixNano() == e.mtime
}

// storeSnap is an immutable view of the store used by one query.
type storeSnap struct {
	srcs   map[string]*srcEntry // by absolute path
	dir    string
	lens   [numBuckets]int64 // committed bucket lengths
	chunks []chunkRec
}

type storeStatus struct {
	building    bool
	root        string
	done, total int64 // bytes of the current pass
	files       int   // files recorded
	disk        int64 // bytes written to the store
}

type store struct {
	dir    string
	lock   *os.File
	snap   atomic.Pointer[storeSnap]
	status atomic.Pointer[storeStatus]
	busy   atomic.Int32 // queries running; indexing waits while > 0
	rootCh chan string
	cancel context.CancelFunc
	done   chan struct{}

	// used only by the indexing goroutine
	srcs        map[string]*srcEntry
	pending     map[string]*srcEntry // indexed but not yet published
	nextFile    int
	st          storeStatus
	lastPublish time.Time
	units       []*unit
	chunks      []chunkRec
	lens        [numBuckets]int64
	bits        []uint64 // trigrams seen in the current chunk (2^24 bits)
	setTris     []uint32
	pairs       []uint64 // trigram<<32 | chunk ID, not yet in the buckets
}

// openStore creates the store directory and starts the indexing goroutine.
func openStore() (*store, error) {
	cleanStaleStores()
	dir, err := os.MkdirTemp("", storePrefix+"*")
	if err != nil {
		return nil, err
	}
	lock, err := lockFile(filepath.Join(dir, "lock"))
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	s := &store{dir: dir, lock: lock, rootCh: make(chan string, 1), done: make(chan struct{}),
		srcs: map[string]*srcEntry{}, pending: map[string]*srcEntry{}}
	s.snap.Store(&storeSnap{srcs: map[string]*srcEntry{}, dir: dir})
	s.status.Store(&storeStatus{})
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go s.run(ctx)
	return s, nil
}

// cleanStaleStores deletes store directories whose psh is no longer running.
func cleanStaleStores() {
	tmp := os.TempDir()
	ents, _ := os.ReadDir(tmp)
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), storePrefix) {
			continue
		}
		dir := filepath.Join(tmp, e.Name())
		lock, err := lockFile(filepath.Join(dir, "lock"))
		if err != nil {
			continue // still in use
		}
		lock.Close()
		os.RemoveAll(dir)
	}
}

// setRoot asks the indexer to index root (the current directory), replacing any
// request not yet started.
func (s *store) setRoot(root string) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return
	}
	select {
	case <-s.rootCh:
	default:
	}
	s.rootCh <- abs
}

// close stops indexing and deletes the store directory.
func (s *store) close() error {
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

func (s *store) run(ctx context.Context) {
	defer close(s.done)
	tick := time.NewTicker(recheckEvery)
	defer tick.Stop()
	root := ""
	for {
		select {
		case <-ctx.Done():
			return
		case root = <-s.rootCh:
		case <-tick.C:
			if root == "" {
				continue
			}
		}
		if err := s.pass(ctx, root); err != nil {
			return
		}
	}
}

type walkedFile struct {
	path string
	fi   fs.FileInfo
}

// pass records every new or changed file below root.
func (s *store) pass(ctx context.Context, root string) error {
	var todo []walkedFile
	var total int64
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p == s.dir {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if e := s.srcs[p]; e != nil && e.valid(fi) {
			return nil
		}
		todo = append(todo, walkedFile{p, fi})
		total += fi.Size()
		return nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.st.building, s.st.root, s.st.done, s.st.total = len(todo) > 0, root, 0, total
	s.publishStatus()
	for _, f := range todo {
		if e, err := s.indexFile(ctx, f); err == nil {
			s.pending[f.path] = e
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(s.lastPublish) > publishEvery {
			if err := s.publish(); err != nil {
				return err
			}
		}
	}
	s.st.building = false
	err := s.publish()
	s.bits = nil // free 2MB until the next pass
	return err
}

// indexFile unpacks one file. Plain text is used in place; text that was
// decompressed is copied to a cache file.
func (s *store) indexFile(ctx context.Context, f walkedFile) (*srcEntry, error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	e := &srcEntry{size: f.fi.Size(), mtime: f.fi.ModTime().UnixNano()}
	pr := &pausingReader{r: fh, s: s, ctx: ctx}
	a := &archiveWalker{ctx: ctx, disp: f.path, onErr: func(error) {},
		leaf: func(member, logical string, br *bufio.Reader, decoded bool) error {
			u := &unit{member: member, logical: logical, id: -1}
			e.units = append(e.units, u)
			if isBinary(br) {
				u.binary = true
				return nil
			}
			c := s.newChunker(u)
			var err error
			if decoded {
				err = s.writeCache(u, br, c)
			} else {
				u.path, u.size = f.path, f.fi.Size()
				_, err = io.CopyBuffer(c, br, make([]byte, copyBufSize))
			}
			if err == nil {
				err = c.close()
			}
			if err != nil {
				s.discardChunk()
			}
			return err
		}}
	if err := a.walkFile(&pausingReaderAt{ra: fh, s: s, ctx: ctx}, f.fi.Size(), pr); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *store) writeCache(u *unit, br *bufio.Reader, c *chunker) error {
	name := filepath.Join(s.dir, fmt.Sprintf(cacheFileName, s.nextFile))
	s.nextFile++
	out, err := os.Create(name)
	if err != nil {
		return err
	}
	n, err := io.CopyBuffer(io.MultiWriter(out, c), br, make([]byte, copyBufSize))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	s.st.disk += n
	if err != nil {
		os.Remove(name)
		return err
	}
	u.path, u.size = name, n
	return nil
}

// publish writes buffered trigrams to the buckets and then makes recorded files
// visible to queries, so a query never sees a file whose chunks aren't indexed yet.
func (s *store) publish() error {
	s.lastPublish = time.Now()
	if err := s.flushPairs(); err != nil {
		return err
	}
	if len(s.pending) > 0 {
		m := make(map[string]*srcEntry, len(s.srcs)+len(s.pending))
		for k, v := range s.srcs {
			m[k] = v
		}
		for k, v := range s.pending {
			m[k] = v
		}
		s.srcs = m
		s.pending = map[string]*srcEntry{}
		n := len(s.chunks)
		s.snap.Store(&storeSnap{srcs: m, dir: s.dir, lens: s.lens, chunks: s.chunks[:n:n]})
	}
	s.st.files = len(s.srcs)
	s.publishStatus()
	return nil
}

func (s *store) publishStatus() {
	st := s.st
	s.status.Store(&st)
}

// wait blocks while a query runs, so indexing doesn't compete for disk and CPU.
func (s *store) wait(ctx context.Context) error {
	for s.busy.Load() > 0 {
		t := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return ctx.Err()
}

type pausingReader struct {
	r   io.Reader
	s   *store
	ctx context.Context
}

func (p *pausingReader) Read(b []byte) (int, error) {
	if err := p.s.wait(p.ctx); err != nil {
		return 0, err
	}
	n, err := p.r.Read(b)
	p.s.st.done += int64(n)
	if time.Since(p.s.lastPublish) > publishEvery {
		p.s.publishStatus()
	}
	return n, err
}

type pausingReaderAt struct {
	ra  io.ReaderAt
	s   *store
	ctx context.Context
}

func (p *pausingReaderAt) ReadAt(b []byte, off int64) (int, error) {
	if err := p.s.wait(p.ctx); err != nil {
		return 0, err
	}
	n, err := p.ra.ReadAt(b, off)
	p.s.st.done += int64(n)
	return n, err
}

// statusText is a short description for the type-ahead hint bar.
func (s *store) statusText() string {
	st := s.status.Load()
	if st == nil {
		return ""
	}
	if st.building && st.total > 0 {
		return fmt.Sprintf("index %d%% · %s", min(100, 100*st.done/st.total), human(st.disk))
	}
	if st.files > 0 {
		return fmt.Sprintf("index ready · %s files · %s", commas(int64(st.files)), human(st.disk))
	}
	return ""
}

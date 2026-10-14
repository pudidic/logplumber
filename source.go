package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

const (
	maxNestDepth   = 6
	maxInMemoryZip = 256 << 20
	batchSize      = 1024
)

var errStop = errors.New("stopped")

// job is one file on disk to read.
type job struct {
	path    string
	disp    string
	size    int64
	isDir   bool
	filters []string // member name globs (empty = everything)
}

var archiveExts = []string{".tar", ".tgz", ".tar.gz", ".tbz", ".tbz2", ".tar.bz2", ".zip", ".jar", ".gz", ".bz2"}

func isArchiveName(name string) bool {
	n := strings.ToLower(name)
	for _, e := range archiveExts {
		if strings.HasSuffix(n, e) {
			return true
		}
	}
	return false
}

func hasGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

func matchAny(pats []string, name string) bool {
	if runtime.GOOS == "windows" {
		name = strings.ToLower(name)
	}
	for _, p := range pats {
		if runtime.GOOS == "windows" {
			p = strings.ToLower(p)
		}
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func dispPath(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	return strings.TrimPrefix(p, "./")
}

// collector gathers jobs from operands, walking directories recursively.
type collector struct {
	ctx     context.Context
	prog    *progress
	exclude string // absolute path of the redirect target
	jobs    []job
	seen    map[string]bool
	dirs    bool // include directories as jobs (find)
	onErr   func(error)
}

func (c *collector) add(j job) {
	if c.seen[j.path] {
		return
	}
	if c.exclude != "" {
		if abs, err := filepath.Abs(j.path); err == nil && abs == c.exclude {
			return
		}
	}
	c.seen[j.path] = true
	c.jobs = append(c.jobs, j)
	c.prog.scanned.Add(1)
	if !j.isDir {
		c.prog.totalBytes.Add(j.size)
	}
}

func (c *collector) walk(root string, filters []string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if c.ctx.Err() != nil {
			return c.ctx.Err()
		}
		if err != nil {
			c.onErr(err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if c.dirs && p != root {
				c.add(job{path: p, disp: dispPath(p), isDir: true})
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			c.onErr(err)
			return nil
		}
		j := job{path: p, disp: dispPath(p), size: info.Size()}
		if len(filters) > 0 && !matchAny(filters, d.Name()) {
			if !isArchiveName(d.Name()) {
				return nil
			}
			j.filters = filters // only matching members of this archive
		}
		c.add(j)
		return nil
	})
}

func (c *collector) addPath(p string) (multi bool, err error) {
	info, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		return true, c.walk(p, nil)
	}
	c.add(job{path: p, disp: dispPath(p), size: info.Size()})
	return isArchiveName(p), nil
}

// collectJobs resolves file operands. With no operands every file below the current
// directory is used. A glob without a path separator (e.g. *.log) matches base names
// recursively, including members of archives.
func collectJobs(ctx context.Context, operands []word, exclude string, prog *progress, onErr func(error)) ([]job, bool, error) {
	c := &collector{ctx: ctx, prog: prog, exclude: exclude, seen: map[string]bool{}, onErr: onErr}
	if len(operands) == 0 {
		return c.jobs, true, c.walk(".", nil)
	}
	multi := len(operands) > 1
	var filters []string
	for _, op := range operands {
		switch {
		case !op.quoted && hasGlob(op.s) && !strings.ContainsAny(op.s, `/\`):
			filters = append(filters, op.s)
			multi = true
		case !op.quoted && hasGlob(op.s):
			ms, err := filepath.Glob(op.s)
			if err != nil {
				return nil, false, err
			}
			if len(ms) == 0 {
				return nil, false, fmt.Errorf("%s: no matching files", op.s)
			}
			multi = true
			for _, m := range ms {
				if _, err := c.addPath(m); err != nil {
					onErr(err)
				}
			}
		default:
			m, err := c.addPath(op.s)
			if err != nil {
				return nil, false, fmt.Errorf("%s: %v", op.s, unwrapPathErr(err))
			}
			multi = multi || m
		}
	}
	if len(filters) > 0 {
		if err := c.walk(".", filters); err != nil {
			return nil, false, err
		}
	}
	return c.jobs, multi, nil
}

func unwrapPathErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// ---------------- progress-counting readers ----------------

type jobCounter struct {
	limit, n int64
	prog     *progress
}

func (c *jobCounter) add(n int64) {
	if c.n+n > c.limit {
		n = c.limit - c.n
	}
	if n > 0 {
		c.n += n
		c.prog.doneBytes.Add(n)
	}
}

func (c *jobCounter) finish() { c.add(c.limit - c.n) }

type countingReader struct {
	r io.Reader
	c *jobCounter
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.c.add(int64(n))
	return n, err
}

type countingReaderAt struct {
	r io.ReaderAt
	c *jobCounter
}

func (r *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.r.ReadAt(p, off)
	r.c.add(int64(n))
	return n, err
}

// ---------------- worker ----------------

type srcMode int

const (
	modeContent srcMode = iota
	modeNames
)

type worker struct {
	ctx    context.Context
	prog   *progress
	mode   srcMode
	find   *findCfg
	stages []Stage
	emits  []Emit // emits[i] feeds stages[i]; emits[len] is the batch sink
	batch  []Line
	out    chan<- []Line
	lines  int64
	onErr  func(error)
}

func newWorker(ctx context.Context, r *runner, specs []*Spec) *worker {
	w := &worker{ctx: ctx, prog: r.prog, mode: r.mode, find: r.find, out: r.batches, onErr: r.reportErr}
	w.stages = make([]Stage, len(specs))
	for i, s := range specs {
		w.stages[i] = s.New()
	}
	w.emits = make([]Emit, len(specs)+1)
	w.emits[len(specs)] = func(l Line) bool {
		w.batch = append(w.batch, l)
		if len(w.batch) >= batchSize {
			return w.flush()
		}
		return true
	}
	for i := len(specs) - 1; i >= 0; i-- {
		st, next := w.stages[i], w.emits[i+1]
		w.emits[i] = func(l Line) bool { return st.Push(l, next) }
	}
	return w
}

func (w *worker) flush() bool {
	if w.lines > 0 {
		w.prog.lines.Add(w.lines)
		w.lines = 0
	}
	if len(w.batch) == 0 {
		return w.ctx.Err() == nil
	}
	select {
	case w.out <- w.batch:
		w.batch = make([]Line, 0, batchSize)
		return true
	case <-w.ctx.Done():
		return false
	}
}

func (w *worker) endFile(src string) bool {
	for i, st := range w.stages {
		if fe, ok := st.(FileEnder); ok {
			if !fe.EndFile(src, w.emits[i+1]) {
				return false
			}
		}
	}
	return true
}

func (w *worker) finish() {
	for i, st := range w.stages {
		st.Finish(w.emits[i+1])
	}
	w.flush()
}

func (w *worker) emitName(name string, isDir bool) bool {
	if !w.find.match(path.Base(name), isDir) {
		return true
	}
	w.lines++
	return w.emits[0](Line{Text: name, Src: name})
}

func (w *worker) doJob(j job) error {
	jc := &jobCounter{limit: j.size, prog: w.prog}
	defer func() {
		jc.finish()
		w.prog.doneFiles.Add(1)
	}()
	if w.mode == modeNames {
		if !w.emitName(j.disp, j.isDir) {
			return errStop
		}
		if j.isDir || !isArchiveName(j.disp) {
			return nil
		}
	}
	f, err := os.Open(j.path)
	if err != nil {
		w.onErr(err)
		return nil
	}
	defer f.Close()
	var magic [4]byte
	if n, _ := f.ReadAt(magic[:], 0); n == 4 && string(magic[:]) == "PK\x03\x04" {
		zr, err := zip.NewReader(&countingReaderAt{f, jc}, j.size)
		if err != nil {
			w.onErr(fmt.Errorf("%s: %v", j.disp, err))
			return nil
		}
		return w.zipMembers(j.disp, zr, 0, j.filters)
	}
	return w.stream(j.disp, path.Base(j.disp), &countingReader{f, jc}, 0, j.filters, true)
}

func (w *worker) zipMembers(disp string, zr *zip.Reader, depth int, filters []string) error {
	for _, zf := range zr.File {
		if strings.HasSuffix(zf.Name, "/") {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			w.onErr(fmt.Errorf("%s::%s: %v", disp, zf.Name, err))
			continue
		}
		err = w.stream(disp+"::"+zf.Name, path.Base(zf.Name), rc, depth+1, filters, false)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// stream sniffs r: archives/compressed data are unpacked recursively, plain text is
// scanned line by line. logical is the name used for member filters.
func (w *worker) stream(disp, logical string, r io.Reader, depth int, filters []string, top bool) error {
	if w.ctx.Err() != nil {
		return errStop
	}
	br := bufio.NewReaderSize(r, 256<<10)
	head, _ := br.Peek(512)
	if depth < maxNestDepth {
		switch {
		case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
			gz, err := gzip.NewReader(br)
			if err != nil {
				w.onErr(fmt.Errorf("%s: %v", disp, err))
				return nil
			}
			defer gz.Close()
			inner := logical
			if l := strings.ToLower(inner); strings.HasSuffix(l, ".tgz") {
				inner = inner[:len(inner)-4] + ".tar"
			} else if strings.HasSuffix(l, ".gz") {
				inner = inner[:len(inner)-3]
			}
			return w.stream(disp, inner, gz, depth+1, filters, top)
		case len(head) >= 4 && string(head[:3]) == "BZh" && head[3] >= '1' && head[3] <= '9':
			inner := logical
			if l := strings.ToLower(inner); strings.HasSuffix(l, ".tbz2") || strings.HasSuffix(l, ".tbz") {
				inner = inner[:strings.LastIndexByte(inner, '.')] + ".tar"
			} else if strings.HasSuffix(l, ".bz2") {
				inner = inner[:len(inner)-4]
			}
			return w.stream(disp, inner, bzip2.NewReader(br), depth+1, filters, top)
		case len(head) >= 4 && string(head[:4]) == "PK\x03\x04":
			data, err := io.ReadAll(io.LimitReader(br, maxInMemoryZip+1))
			if err != nil || len(data) > maxInMemoryZip {
				w.onErr(fmt.Errorf("%s: nested zip too large or unreadable", disp))
				return nil
			}
			zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				w.onErr(fmt.Errorf("%s: %v", disp, err))
				return nil
			}
			return w.zipMembers(disp, zr, depth, filters)
		case len(head) >= 262 && string(head[257:262]) == "ustar":
			tr := tar.NewReader(br)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					w.onErr(fmt.Errorf("%s: %v", disp, err))
					return nil
				}
				if !hdr.FileInfo().Mode().IsRegular() {
					continue
				}
				name := strings.TrimPrefix(hdr.Name, "./")
				if err := w.stream(disp+"::"+name, path.Base(name), tr, depth+1, filters, false); err != nil {
					return err
				}
			}
		}
	}
	// leaf
	if w.mode == modeNames {
		if !top {
			if !w.emitName(disp, false) {
				return errStop
			}
		}
		return nil
	}
	if len(filters) > 0 && !matchAny(filters, logical) {
		return nil
	}
	if p, _ := br.Peek(8000); bytes.IndexByte(p, 0) >= 0 {
		w.prog.skipped.Add(1)
		return nil
	}
	return w.scanText(disp, br)
}

func (w *worker) scanText(disp string, br *bufio.Reader) error {
	no := 0
	var acc []byte
	entry := w.emits[0]
	for {
		b, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			acc = append(acc, b...)
			continue
		}
		if len(acc) > 0 {
			acc = append(acc, b...)
			b = acc
		}
		if len(b) > 0 {
			raw := len(b)
			t := b
			if t[len(t)-1] == '\n' {
				t = t[:len(t)-1]
			}
			if len(t) > 0 && t[len(t)-1] == '\r' {
				t = t[:len(t)-1]
			}
			no++
			w.lines++
			if !entry(Line{Text: string(t), Src: disp, No: no, Raw: raw}) {
				return errStop
			}
			if no&1023 == 0 {
				if w.ctx.Err() != nil {
					return errStop
				}
				w.prog.lines.Add(w.lines)
				w.lines = 0
			}
		}
		acc = acc[:0]
		if err != nil {
			if err == io.EOF {
				break
			}
			w.onErr(fmt.Errorf("%s: %v", disp, err))
			break
		}
	}
	if !w.endFile(disp) {
		return errStop
	}
	return nil
}

// ---------------- progress ----------------

type progress struct {
	scanned    atomic.Int64
	totalBytes atomic.Int64
	doneBytes  atomic.Int64
	doneFiles  atomic.Int64
	skipped    atomic.Int64
	lines      atomic.Int64
	outLines   atomic.Int64
	errors     atomic.Int64
	phase      atomic.Int32 // 0 scanning, 1 reading, 2 finalizing
	finalizing atomic.Value // string: name of the stage being finalized
}

func (p *progress) percent() float64 {
	switch p.phase.Load() {
	case 0:
		return 0
	}
	t := p.totalBytes.Load()
	if t == 0 {
		if p.phase.Load() >= 2 {
			return 100
		}
		total := p.scanned.Load()
		if total == 0 {
			return 0
		}
		return 100 * float64(p.doneFiles.Load()) / float64(total)
	}
	v := 100 * float64(p.doneBytes.Load()) / float64(t)
	return min(v, 100)
}

package main

import (
	"bufio"
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
		if !d.Type().IsRegular() || d.Name() == ".psh.conf" { // configuration, not data
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
		case (op.nameOnly || !op.quoted && hasGlob(op.s)) && !strings.ContainsAny(op.s, `/\`):
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
	snap   *storeSnap // index snapshot, nil without a store
}

func newWorker(ctx context.Context, r *runner, specs []*Spec) *worker {
	w := &worker{ctx: ctx, prog: r.prog, mode: r.mode, find: r.find, out: r.batches, onErr: r.reportErr, snap: r.snap}
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
	if e := w.cached(j); e != nil {
		return w.doCached(j, e, jc)
	}
	f, err := os.Open(j.path)
	if err != nil {
		w.onErr(err)
		return nil
	}
	defer f.Close()
	a := &archiveWalker{ctx: w.ctx, disp: j.disp, onErr: w.onErr,
		leaf: func(member, logical string, br *bufio.Reader, _ bool) error {
			disp := j.disp + member
			if w.mode == modeNames {
				if member != "" && !w.emitName(disp, false) {
					return errStop
				}
				return nil
			}
			if len(j.filters) > 0 && !matchAny(j.filters, logical) {
				return nil
			}
			if isBinary(br) {
				w.prog.skipped.Add(1)
				return nil
			}
			if err := w.scanText(disp, br, 1); err != nil {
				return err
			}
			if !w.endFile(disp) {
				return errStop
			}
			return nil
		}}
	return a.walkFile(&countingReaderAt{f, jc}, j.size, &countingReader{f, jc})
}

// cached returns the store entry for the job's file if it is still valid.
func (w *worker) cached(j job) *srcEntry {
	if w.snap == nil {
		return nil
	}
	abs, err := filepath.Abs(j.path)
	if err != nil {
		return nil
	}
	e := w.snap.srcs[abs]
	if e == nil {
		return nil
	}
	if fi, err := os.Stat(j.path); err != nil || !e.valid(fi) {
		return nil
	}
	return e
}

// doCached reads a file through the store: archive members come from their
// decompressed copies, so nothing is unpacked again.
func (w *worker) doCached(j job, e *srcEntry, jc *jobCounter) error {
	var units []*unit
	var total int64
	for _, u := range e.units {
		if len(j.filters) > 0 && !matchAny(j.filters, u.logical) {
			continue
		}
		units = append(units, u)
		total += u.size
	}
	scale := 1.0
	if total > 0 {
		scale = float64(j.size) / float64(total)
	}
	for _, u := range units {
		disp := j.disp + u.member
		if w.mode == modeNames {
			if u.member != "" && !w.emitName(disp, false) {
				return errStop
			}
			continue
		}
		if u.binary {
			w.prog.skipped.Add(1)
			continue
		}
		f, err := os.Open(u.path)
		if err != nil {
			w.onErr(fmt.Errorf("%s: %v", disp, err))
			continue
		}
		err = w.scanText(disp, bufio.NewReaderSize(&scaledReader{r: f, c: jc, scale: scale}, 256<<10), 1)
		f.Close()
		if err != nil {
			return err
		}
		if !w.endFile(disp) {
			return errStop
		}
	}
	return nil
}

// scaledReader reports progress in units of the original (compressed) file.
type scaledReader struct {
	r     io.Reader
	c     *jobCounter
	scale float64
	frac  float64
}

func (s *scaledReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.frac += float64(n) * s.scale
	whole := int64(s.frac)
	s.frac -= float64(whole)
	s.c.add(whole)
	return n, err
}

// scanText feeds the lines of br into the worker's stages; no is the number of
// the first line.
func (w *worker) scanText(disp string, br *bufio.Reader, no int) error {
	no--
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
				return nil
			}
			w.onErr(fmt.Errorf("%s: %v", disp, err))
			return nil
		}
	}
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
	phase      atomic.Int32 // 0 scanning, 1 reading, 2 finalizing, 3 following
	following  atomic.Int64 // files followed (real-time)
	newLines   atomic.Int64 // lines written to them while following
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

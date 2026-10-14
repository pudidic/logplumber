package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// Real-time tracking ("follow" mode)
//
// SPL:   earliest=rt                 only new events, as they are written
//        earliest=rt-5m latest=rt    the last 5 minutes first, then new events, in a
//                                    sliding 5-minute window (stats and timechart
//                                    only count events inside it)
// Shell: tail -f [-n N] [file...]    the last N lines of each file, then new lines
//
// After the start, the files are polled for appended lines; files that appear
// later are read from the beginning, and a file that shrinks (truncated or
// rotated) is read again from the start. Archives are not followed. Ctrl+C stops
// following; the results so far are then finished as usual.

type followCfg struct {
	window   time.Duration // sliding window (0: keep everything)
	backfill bool          // read the existing data first (SPL rt-<window>)
	tailN    int           // shell tail -f: start with the last N lines of each file
	shell    bool
}

const (
	followPoll   = 250 * time.Millisecond
	followRescan = 2 * time.Second
	followChunk  = 8 << 20 // bytes read from one file per poll
)

type followed struct {
	path, disp string
	off        int64 // bytes consumed (complete lines and the partial line)
	partial    []byte
	line       int // number of the last complete line
	binary     bool
}

// followSource reads the plain files of jobs and then keeps reading what is
// appended to them until ctx is cancelled.
func (r *runner) followSource(ctx context.Context, jobs []job) {
	// The worker must still flush its last batch after Ctrl+C stopped following.
	w := newWorker(context.WithoutCancel(ctx), r, r.workerSpecs)
	files := map[string]*followed{}
	own := ownOutputs()
	add := func(j job, fromStart bool) {
		if j.isDir || isArchiveName(j.disp) || files[j.path] != nil || isOwnOutput(j.path, own) {
			return
		}
		f := &followed{path: j.path, disp: j.disp}
		files[j.path] = f
		if fromStart {
			return
		}
		size := j.size
		if st, err := os.Stat(j.path); err == nil {
			size = st.Size() // j.size may come from the type-ahead file-list cache
		}
		f.off, f.line, f.binary = startOffset(j.path, size)
	}
	backfill := r.follow.backfill
	for _, j := range jobs {
		add(j, backfill)
	}
	if n := r.follow.tailN; n > 0 {
		for _, f := range sortedFollowed(files) {
			r.tailLines(w, f, n)
		}
	}
	r.prog.phase.Store(3)
	poll := time.NewTicker(followPoll)
	defer poll.Stop()
	lastScan := time.Now()
	first := true
	for {
		any := false
		for _, f := range sortedFollowed(files) {
			if r.readNew(w, f, first && backfill) {
				any = true
			}
		}
		first = false
		r.prog.following.Store(int64(len(files)))
		if any {
			w.flush()
		}
		select {
		case <-ctx.Done():
			w.finish()
			return
		case <-poll.C:
		}
		if time.Since(lastScan) >= followRescan {
			lastScan = time.Now()
			now, err := followJobs(ctx, r.ops, r.exclude, &progress{})
			if err == nil {
				seen := map[string]bool{}
				for _, j := range now {
					seen[j.path] = true
					add(j, true) // a new file: read it from the start
				}
				for p := range files {
					if !seen[p] {
						delete(files, p)
					}
				}
			}
		}
	}
}

// ownOutputs are the files psh's stdout and stderr write to: following them would
// read psh's own output back in (psh -c 'earliest=rt' > out.log in a log folder).
func ownOutputs() []os.FileInfo {
	var out []os.FileInfo
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
			out = append(out, fi)
		}
	}
	return out
}

func isOwnOutput(path string, own []os.FileInfo) bool {
	if len(own) == 0 {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	for _, o := range own {
		if os.SameFile(fi, o) {
			return true
		}
	}
	return false
}

// followJobs collects the files to follow. Operands that do not exist (yet) are
// skipped: they are picked up when they appear, as with tail -F.
func followJobs(ctx context.Context, ops []word, exclude string, prog *progress) ([]job, error) {
	if len(ops) == 0 {
		jobs, _, err := collectJobs(ctx, nil, exclude, prog, func(error) {})
		return jobs, err
	}
	var all []job
	for _, op := range ops {
		jobs, _, err := collectJobs(ctx, []word{op}, exclude, prog, func(error) {})
		if err == nil {
			all = append(all, jobs...)
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return all, nil
}

func sortedFollowed(m map[string]*followed) []*followed {
	out := make([]*followed, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].disp < out[j].disp })
	return out
}

// startOffset returns where following a file starts: its end, counting its lines
// so that new lines get the right numbers. A file that looks binary is skipped.
func startOffset(path string, size int64) (int64, int, bool) {
	fh, err := os.Open(path)
	if err != nil {
		return size, 0, false
	}
	defer fh.Close()
	buf := make([]byte, 256<<10)
	var off int64
	lines := 0
	for off < size {
		n, err := fh.ReadAt(buf[:min(int64(len(buf)), size-off)], off)
		if off == 0 && bytes.IndexByte(buf[:min(n, 8192)], 0) >= 0 {
			return size, 0, true
		}
		lines += bytes.Count(buf[:n], []byte{'\n'})
		off += int64(n)
		if err != nil {
			break
		}
	}
	return size, lines, false
}

// readNew reads the complete lines appended to a file since the last call and
// sends them through the worker. It reports whether there were any.
func (r *runner) readNew(w *worker, f *followed, backfill bool) bool {
	if f.binary {
		return false
	}
	st, err := os.Stat(f.path)
	if err != nil {
		return false
	}
	size := st.Size()
	if size < f.off { // truncated or replaced: start over
		f.off, f.partial, f.line = 0, nil, 0
	}
	if size == f.off {
		return false
	}
	fh, err := os.Open(f.path)
	if err != nil {
		return false
	}
	defer fh.Close()
	sent := false
	for f.off < size { // in chunks, so a large backfill does not need much memory
		n := min(size-f.off, followChunk)
		buf := make([]byte, n)
		got, err := fh.ReadAt(buf, f.off)
		if err != nil && err != io.EOF || got == 0 {
			break
		}
		buf = buf[:got]
		if f.off == 0 && bytes.IndexByte(buf[:min(len(buf), 8192)], 0) >= 0 {
			f.binary = true
			r.prog.skipped.Add(1)
			return false
		}
		f.off += int64(got)
		data := append(f.partial, buf...)
		last := bytes.LastIndexByte(data, '\n')
		if last < 0 {
			f.partial = data
			continue
		}
		f.partial = append([]byte(nil), data[last+1:]...)
		for _, b := range bytes.Split(data[:last], []byte{'\n'}) {
			f.line++
			raw := len(b) + 1
			b = bytes.TrimSuffix(b, []byte{'\r'})
			w.lines++
			if !backfill {
				r.prog.newLines.Add(1)
			}
			if !w.emits[0](Line{Text: string(b), Src: f.disp, No: f.line, Raw: raw}) {
				return sent
			}
			sent = true
		}
		w.endFile(f.disp)
		w.flush()
	}
	return sent
}

// tailLines sends the last n lines of a file (tail -f) and moves the file's
// position to its end.
func (r *runner) tailLines(w *worker, f *followed, n int) {
	if f.binary || f.off == 0 {
		return
	}
	fh, err := os.Open(f.path)
	if err != nil {
		return
	}
	defer fh.Close()
	// read backwards until n+1 newlines are seen (the file usually ends with one)
	var tail []byte
	pos := f.off
	for pos > 0 && bytes.Count(tail, []byte{'\n'}) <= n {
		step := min(pos, 64<<10)
		pos -= step
		chunk := make([]byte, step)
		if _, err := fh.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return
		}
		tail = append(chunk, tail...)
	}
	tail = bytes.TrimSuffix(tail, []byte{'\n'})
	lines := bytes.Split(tail, []byte{'\n'})
	if pos > 0 || len(lines) > n {
		lines = lines[max(0, len(lines)-n):]
	}
	no := f.line - len(lines)
	for _, b := range lines {
		no++
		w.lines++
		if !w.emits[0](Line{Text: string(bytes.TrimSuffix(b, []byte{'\r'})), Src: f.disp, No: no, Raw: len(b) + 1}) {
			break
		}
	}
	w.endFile(f.disp)
	w.flush()
}

// ---------------- sliding window (SPL earliest=rt-<span>) ----------------

type windowStage struct {
	window time.Duration
	lines  []Line
	times  []int64 // _time, or the arrival time for events without one
}

func (s *windowStage) Push(l Line, emit Emit) bool {
	ms, ok := l.lineTime()
	if !ok {
		ms = time.Now().UnixMilli()
	}
	s.lines = append(s.lines, l)
	s.times = append(s.times, ms)
	if len(s.lines)&1023 == 0 {
		s.evict()
	}
	return true
}

func (s *windowStage) evict() {
	cut := time.Now().Add(-s.window).UnixMilli()
	j := 0
	for i := range s.lines {
		if s.times[i] >= cut {
			s.lines[j], s.times[j] = s.lines[i], s.times[i]
			j++
		}
	}
	clear(s.lines[j:])
	s.lines, s.times = s.lines[:j], s.times[:j]
}

func (s *windowStage) emitAll(emit Emit) {
	s.evict()
	for _, l := range s.lines {
		if !emit(l) {
			return
		}
	}
}

func (s *windowStage) Snapshot(emit Emit) { s.emitAll(emit) }
func (s *windowStage) Finish(emit Emit)   { s.emitAll(emit) }

func windowSpec(d time.Duration) *Spec {
	return &Spec{Name: fmt.Sprintf("window %s", d), Blocking: true, New: func() Stage { return &windowStage{window: d} }}
}

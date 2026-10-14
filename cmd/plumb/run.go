package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// display is where a running pipeline sends its results and progress: either the
// normal scrolling terminal (UI) or the type-ahead live screen (liveRun).
type display interface {
	Begin()
	SetStatus(s string)
	SetLive(lines []string) // preview of a blocking stage (nil clears it)
	Print(lines []Line)     // result lines; the display takes ownership of the slice
	Errorf(format string, a ...any)
	End(summary string)
	Live() bool       // whether previews should be computed
	PreviewRows() int // number of preview lines that fit
	SetTimeline(t *timeline)
	// SetColumns says whether the events' file names are a column, and the
	// column width to start with (it covers every file the run will read).
	SetColumns(src bool, width int)
}

var errInterrupted = errors.New("interrupted (Ctrl+C)")

type srcKind int

const (
	kindContent srcKind = iota
	kindNames
	kindStatic
)

type runner struct {
	sh          *Shell
	ui          display
	live        bool // started from the type-ahead editor (enables the job cache)
	prog        *progress
	start       time.Time
	kind        srcKind
	mode        srcMode
	find        *findCfg
	static      []string
	specs       []*Spec
	workerSpecs []*Spec // run in each worker thread
	serialSpecs []*Spec // run in one goroutine after the workers
	ops         []word
	batches     chan []Line
	snap        *storeSnap           // index snapshot taken when the run starts
	follow      *followCfg           // real-time: keep reading what is appended
	timeline    *timeline            // event search: events per time slot
	srcCol      bool                 // SPL events: show their file names as a column
	colWidth    int                  // file column width that fits every file read
	need        []needAlt            // what a matching line must contain (index)
	cand        map[int32][]chunkRec // indexed units: the chunks to read (nil: all)
	exclude     string
	errMu       sync.Mutex
	errN        int
}

func (r *runner) reportErr(err error) {
	r.prog.errors.Add(1)
	r.errMu.Lock()
	r.errN++
	n := r.errN
	r.errMu.Unlock()
	if n <= 20 {
		r.ui.Errorf("plumb: %v", err)
	} else if n == 21 {
		r.ui.Errorf("plumb: (more errors suppressed)")
	}
}

// plan turns the parsed pipeline into a source and a list of stage specs.
func (sh *Shell) plan(ast *pipelineAST, disp display) (*runner, error) {
	r := &runner{sh: sh, ui: disp, prog: &progress{}}
	if ast.spl {
		return r, r.planSPL(ast)
	}
	first := ast.cmds[0]
	rest := ast.cmds
	switch first.args[0].s {
	case "find":
		f, err := parseFind(first.args[1:])
		if err != nil {
			return nil, err
		}
		r.kind, r.mode, r.find = kindNames, modeNames, f
		rest = ast.cmds[1:]
	case "ls":
		lines, err := listDir(first.args[1:])
		if err != nil {
			return nil, err
		}
		r.kind, r.static = kindStatic, lines
		rest = ast.cmds[1:]
	case "echo":
		parts := make([]string, len(first.args)-1)
		for i, a := range first.args[1:] {
			parts[i] = a.s
		}
		r.kind, r.static = kindStatic, []string{strings.Join(parts, " ")}
		rest = ast.cmds[1:]
	}
	for i, c := range rest {
		switch c.args[0].s {
		case "find", "ls", "echo":
			return nil, fmt.Errorf("%s: only allowed as the first command of a pipeline", c.args[0].s)
		}
		s, ops, err := buildSpec(c, i == 0 && r.kind == kindContent)
		if err != nil {
			return nil, err
		}
		if len(ops) > 0 {
			if i > 0 || r.kind != kindContent {
				return nil, fmt.Errorf("%s: file operands are only allowed on the first command", s.Name)
			}
			r.ops = ops
		}
		if i == 0 && r.kind == kindContent {
			r.need = s.need
		}
		if s.rt != nil {
			if i > 0 || r.kind != kindContent {
				return nil, fmt.Errorf("tail -f: only allowed as the first command (tail -f file | grep ...)")
			}
			r.follow = s.rt
		}
		r.specs = append(r.specs, s)
	}
	if ast.redirect != "" {
		if abs, err := filepath.Abs(ast.redirect); err == nil {
			r.exclude = abs
		}
	}
	return r, nil
}

// planSPL builds the stages of an SPL query.
func (r *runner) planSPL(ast *pipelineAST) error {
	for i, c := range ast.cmds {
		s, ops, err := buildSPLSpec(c, i == 0)
		if err != nil {
			return err
		}
		if i == 0 {
			r.ops = ops
		}
		if s.out != "" {
			if abs, err := filepath.Abs(s.out); err == nil {
				r.exclude = abs // don't read the file being written
			}
		}
		r.specs = append(r.specs, s)
	}
	if len(r.specs) > 0 {
		r.follow = r.specs[0].rt
	}
	r.need = r.specs[0].need
	_, typeAhead := r.ui.(*liveRun) // a preview while typing, not the full output
	r.specs, r.timeline = orderSPL(r.specs, typeAhead, r.follow)
	r.srcCol = true // events, unless a command makes a table of them
	for _, s := range r.specs {
		if tableSPL[s.Name] {
			r.srcCol = false
		}
	}
	return nil
}

// columnHint is a file column width that fits the labels of all files a run
// reads, so a piped or scrolling output stays aligned. Members of archives come
// from the index when it has them; otherwise an archive gets the widest column.
// With grep -n the line number gets the digits of the largest file's size.
func (r *runner) columnHint(jobs []job) int {
	if len(r.specs) == 0 || !r.srcCol && !r.specs[0].labels {
		return 0
	}
	w, size := 0, int64(0)
	for _, j := range jobs {
		size = max(size, j.size)
		if !isArchiveName(j.disp) {
			w = max(w, strWidth([]rune(j.disp)))
			continue
		}
		e := r.cachedEntry(j.path)
		if e == nil {
			return maxSrcColumn
		}
		for _, u := range e.units {
			w = max(w, strWidth([]rune(j.disp+u.member)))
			size = max(size, u.size)
		}
	}
	if r.specs[0].numbered {
		w += 1 + len(strconv.FormatInt(max(size, 1), 10))
	}
	return min(w, maxSrcColumn)
}

// cachedEntry returns the index entry of an unchanged file, or nil.
func (r *runner) cachedEntry(path string) *srcEntry {
	if r.snap == nil {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	e := r.snap.srcs[abs]
	if fi, err := os.Stat(path); e == nil || err != nil || !e.valid(fi) {
		return nil
	}
	return e
}

func (r *runner) setup(multi bool) {
	for i, s := range r.specs {
		if s.Setup != nil {
			s.Setup(planCtx{first: i == 0 && r.kind == kindContent, multi: multi}, s)
		}
	}
	p := 0
	if r.kind != kindStatic {
		for p < len(r.specs) && r.specs[p].Parallel {
			p++
		}
	}
	r.workerSpecs, r.serialSpecs = r.specs[:p:p], r.specs[p:]
	// An aggregation right after the parallel part is pre-aggregated per worker.
	if r.kind != kindStatic && p < len(r.specs) && r.specs[p].NewPartial != nil {
		s := r.specs[p]
		r.workerSpecs = append(r.workerSpecs, &Spec{Name: s.Name, Parallel: true, New: s.NewPartial})
	}
}

func instantiate(specs []*Spec) ([]Stage, []Emit) {
	st := make([]Stage, len(specs))
	for i, s := range specs {
		st[i] = s.New()
	}
	return st, make([]Emit, len(specs)+1)
}

func chain(st []Stage, emits []Emit, sink Emit) {
	emits[len(st)] = sink
	for i := len(st) - 1; i >= 0; i-- {
		s, next := st[i], emits[i+1]
		emits[i] = func(l Line) bool { return s.Push(l, next) }
	}
}

func (sh *Shell) runPipeline(userCtx context.Context, ast *pipelineAST, disp display, live bool) error {
	r, err := sh.plan(ast, disp)
	if err != nil {
		return err
	}
	r.start = time.Now()
	r.live = live
	if sh.store != nil {
		sh.store.busy.Add(1) // indexing pauses while a query runs
		defer sh.store.busy.Add(-1)
		r.snap = sh.store.snap.Load()
	}

	if r.follow != nil {
		r.snap = nil // read the files themselves
	}
	// In real-time mode Ctrl+C (userCtx) only stops following; the results so far
	// are still finished and shown.
	runCtx := userCtx
	if r.follow != nil {
		runCtx = context.WithoutCancel(userCtx)
	}
	sink := &outSink{ui: r.ui, ctx: runCtx, prog: r.prog}
	if ast.redirect != "" {
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if ast.appendTo {
			flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(ast.redirect, flag, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		bw := bufio.NewWriterSize(f, 256<<10)
		defer bw.Flush()
		sink.file = bw
	}

	srcCtx, cancelSrc := context.WithCancel(userCtx)
	defer cancelSrc()
	r.batches = make(chan []Line, 4*sh.threads)

	r.ui.Begin()
	r.ui.SetTimeline(r.timeline)
	stopStatus := make(chan struct{})
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		spin := 0
		for {
			select {
			case <-stopStatus:
				return
			case <-t.C:
				spin++
				r.ui.SetStatus(r.statusLine(spin))
			}
		}
	}()

	ready := make(chan error, 1)
	go r.source(srcCtx, ready)
	if err := <-ready; err != nil {
		close(stopStatus)
		<-statusDone
		r.ui.End("")
		return err
	}
	r.ui.SetColumns(r.srcCol, r.colWidth) // the files are known now

	stoppedEarly := r.serial(runCtx, cancelSrc, sink)
	sink.flush()
	close(stopStatus)
	<-statusDone

	r.ui.End(r.summary(runCtx.Err() != nil, stoppedEarly, ast.redirect))
	if runCtx.Err() != nil {
		return errInterrupted
	}
	return nil
}

// ---------------- job cache (type-ahead mode) ----------------

type jobCache struct {
	key   string
	jobs  []job
	multi bool
	bytes int64
	at    time.Time
}

const jobCacheTTL = 10 * time.Second

func (r *runner) cacheKey() string {
	wd, _ := os.Getwd()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%d\x00%s", wd, r.kind, r.exclude)
	for _, o := range r.ops {
		fmt.Fprintf(&b, "\x00%s\x01%v", o.s, o.quoted)
	}
	if r.find != nil {
		fmt.Fprintf(&b, "\x00%q\x00%c", r.find.roots, r.find.typ)
	}
	return b.String()
}

func (sh *Shell) cachedJobs(key string) *jobCache {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if c := sh.jobs; c != nil && c.key == key && time.Since(c.at) < jobCacheTTL {
		return c
	}
	return nil
}

func (sh *Shell) storeJobs(c *jobCache) {
	sh.mu.Lock()
	sh.jobs = c
	sh.mu.Unlock()
}

func (sh *Shell) dropJobCache() { sh.storeJobs(nil) }

// source collects input jobs, then reads them with a pool of worker threads.
func (r *runner) source(ctx context.Context, ready chan<- error) {
	defer close(r.batches)
	if r.kind == kindStatic {
		r.setup(false)
		ready <- nil
		r.prog.phase.Store(1)
		b := make([]Line, len(r.static))
		for i, s := range r.static {
			b[i] = Line{Text: s}
		}
		r.prog.lines.Add(int64(len(b)))
		select {
		case r.batches <- b:
		case <-ctx.Done():
		}
		return
	}
	var jobs []job
	var multi bool
	var err error
	key := ""
	if r.live {
		key = r.cacheKey()
	}
	if c := r.sh.cachedJobs(key); r.live && c != nil {
		jobs, multi = c.jobs, c.multi
		r.prog.scanned.Store(int64(len(jobs)))
		r.prog.totalBytes.Store(c.bytes)
	} else {
		if r.kind == kindNames {
			c := &collector{ctx: ctx, prog: r.prog, exclude: r.exclude, seen: map[string]bool{}, dirs: r.find.typ != 'f', onErr: r.reportErr}
			for _, root := range r.find.roots {
				if _, e := os.Stat(root); e != nil {
					err = fmt.Errorf("%s: %v", root, unwrapPathErr(e))
					break
				}
				if e := c.walk(root, nil); e != nil {
					err = e
					break
				}
			}
			jobs, multi = c.jobs, true
		} else {
			if r.follow != nil {
				jobs, err = followJobs(ctx, r.ops, r.exclude, r.prog)
				multi = true
			} else {
				jobs, multi, err = collectJobs(ctx, r.ops, r.exclude, r.prog, r.reportErr)
			}
		}
		if err == nil && r.live && ctx.Err() == nil {
			r.sh.storeJobs(&jobCache{key: key, jobs: jobs, multi: multi, bytes: r.prog.totalBytes.Load(), at: time.Now()})
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			err = errInterrupted
		}
		ready <- err
		return
	}
	r.setup(multi)
	r.colWidth = r.columnHint(jobs)
	ready <- nil
	r.prog.phase.Store(1)
	if r.follow != nil {
		r.followSource(ctx, jobs)
		return
	}
	if r.snap != nil && r.need != nil {
		if cand, ok := r.snap.chunkSet(r.need); ok {
			r.cand = cand // indexed files: read only the chunks that may match
		}
	}

	jobCh := make(chan job)
	var wg sync.WaitGroup
	for i := 0; i < r.sh.threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newWorker(ctx, r, r.workerSpecs)
			for j := range jobCh {
				if w.doJob(j) == errStop {
					break
				}
			}
			if ctx.Err() == nil {
				w.finish()
			}
			for range jobCh { // drain after stop
			}
		}()
	}
feed:
	for _, j := range jobs {
		select {
		case jobCh <- j:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobCh)
	wg.Wait()
}

// serial runs the order-dependent part of the pipeline in a single goroutine and
// renders live previews of blocking stages. It returns true if a stage (e.g. head)
// stopped the input early.
func (r *runner) serial(userCtx context.Context, cancelSrc context.CancelFunc, sink *outSink) bool {
	specs := r.serialSpecs
	st, emits := instantiate(specs)
	chain(st, emits, sink.emit)
	blockAt := -1
	for i, s := range specs {
		if s.Blocking {
			blockAt = i
			break
		}
	}
	stopped := false
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	nextPreview := time.Now().Add(150 * time.Millisecond)
loop:
	for {
		select {
		case b, ok := <-r.batches:
			if !ok {
				break loop
			}
			if stopped {
				continue
			}
			for _, l := range b {
				if !emits[0](l) {
					stopped = true
					cancelSrc()
					break
				}
			}
		case <-tick.C:
		}
		sink.flushIfDue()
		if blockAt >= 0 && r.ui.Live() && time.Now().After(nextPreview) {
			t0 := time.Now()
			r.preview(specs, st, blockAt)
			cost := time.Since(t0)
			nextPreview = time.Now().Add(max(250*time.Millisecond, 4*cost))
		}
	}
	if userCtx.Err() != nil {
		return stopped
	}
	r.prog.phase.Store(2)
	if blockAt >= 0 {
		r.prog.finalizing.Store(specs[blockAt].Name)
	}
	r.ui.SetLive(nil)
	for i, s := range st {
		s.Finish(emits[i+1])
	}
	return stopped
}

// preview renders the current state of the first blocking stage, passed through
// fresh copies of the stages after it.
func (r *runner) preview(specs []*Spec, st []Stage, b int) {
	snap, ok := st[b].(Snapshotter)
	if !ok {
		return
	}
	pv := &previewBuf{rows: max(3, r.ui.PreviewRows()), srcCol: r.srcCol}
	down, emits := instantiate(specs[b+1:])
	chain(down, emits, pv.add)
	snap.Snapshot(emits[0])
	for i, s := range down {
		s.Finish(emits[i+1])
	}
	r.ui.SetLive(pv.render(specs[b].Name))
}

type previewBuf struct {
	header *tableHeader
	rows   int
	head   []Line
	last   Line
	count  int
	cut    bool
	srcCol bool
}

func (p *previewBuf) add(l Line) bool {
	if p.header == nil && l.Rec != nil {
		p.header = l.Rec.header
	}
	p.count++
	if len(p.head) < p.rows {
		p.head = append(p.head, l)
	}
	p.last = l
	if p.count >= 200000 {
		p.cut = true
		return false
	}
	return true
}

func (p *previewBuf) render(stage string) []string {
	more := ""
	if p.cut {
		more = "+"
	}
	out := []string{fmt.Sprintf("── live preview (%s, %s%s lines) ──", stage, commas(int64(p.count)), more)}
	if p.header != nil {
		out = append(out, p.header.text, p.header.rule)
	}
	w := columnWidth(append(p.head, p.last), p.srcCol)
	show := func(ls ...Line) {
		for _, l := range ls {
			out = append(out, displayLine(l, p.srcCol, w))
		}
	}
	if p.count <= p.rows {
		show(p.head...)
		return out
	}
	show(p.head[:p.rows-2]...)
	out = append(out, fmt.Sprintf("   … %s%s more lines …", commas(int64(p.count-p.rows+1)), more))
	if !p.cut {
		show(p.last)
	}
	return out
}

// ---------------- output sink ----------------

type outSink struct {
	ui        display
	ctx       context.Context
	prog      *progress
	file      *bufio.Writer
	header    *tableHeader // last table header written to the file
	pending   []Line
	lastFlush time.Time
	n         int
}

func (o *outSink) emit(l Line) bool {
	if o.ctx.Err() != nil {
		return false
	}
	o.prog.outLines.Add(1)
	if o.file != nil {
		if h := lineHeader(l); h != nil && h != o.header {
			o.header = h
			o.file.WriteString(h.text + "\n" + h.rule + "\n")
		}
		o.file.WriteString(l.Text)
		o.file.WriteByte('\n')
		return true
	}
	o.pending = append(o.pending, l)
	o.n++
	if len(o.pending) >= 4096 || o.n&255 == 0 {
		o.flushIfDue()
	}
	return true
}

// lineHeader returns the table header of a row produced by stats, top or table.
func lineHeader(l Line) *tableHeader {
	if l.Rec != nil {
		return l.Rec.header
	}
	return nil
}

func (o *outSink) flushIfDue() {
	if len(o.pending) >= 4096 || time.Since(o.lastFlush) > 40*time.Millisecond {
		o.flush()
	}
}

func (o *outSink) flush() {
	o.lastFlush = time.Now()
	if len(o.pending) == 0 {
		return
	}
	o.ui.Print(o.pending)
	o.pending = nil
}

// ---------------- status ----------------

func (r *runner) statusLine(spin int) string {
	p := r.prog
	el := time.Since(r.start)
	sp := `|/-\`[spin%4]
	switch p.phase.Load() {
	case 0:
		return fmt.Sprintf("%c scanning files… %s", sp, commas(p.scanned.Load()))
	case 3:
		return fmt.Sprintf("%c following %s files · %s new lines · out %s · %.0fs — Ctrl+C stops",
			sp, commas(p.following.Load()), commas(p.newLines.Load()), commas(p.outLines.Load()), el.Seconds())
	case 2:
		name, _ := p.finalizing.Load().(string)
		s := fmt.Sprintf("%s 100.0%%  %c finalizing", bar(100), sp)
		if name != "" {
			s += " (" + name + ")"
		}
		return s + fmt.Sprintf("  out %s  %.1fs", commas(p.outLines.Load()), el.Seconds())
	}
	pct := p.percent()
	done := p.doneBytes.Load()
	rate := float64(done) / max(el.Seconds(), 0.001)
	return fmt.Sprintf("%s %5.1f%%  %c files %s/%s  %s/%s  %s/s  lines %s  out %s  %.1fs  %d threads",
		bar(pct), pct, sp,
		commas(p.doneFiles.Load()), commas(p.scanned.Load()),
		human(done), human(p.totalBytes.Load()), human(int64(rate)),
		commas(p.lines.Load()), commas(p.outLines.Load()), el.Seconds(), r.sh.threads)
}

func (r *runner) summary(interrupted, early bool, redirect string) string {
	p := r.prog
	pct := p.percent()
	if p.phase.Load() >= 2 && !early {
		pct = 100
	}
	state := "done"
	switch {
	case r.follow != nil:
		return fmt.Sprintf("stopped following · %s files · %s new lines · %s lines out · %.0fs",
			commas(p.following.Load()), commas(p.newLines.Load()), commas(p.outLines.Load()), time.Since(r.start).Seconds())
	case interrupted:
		state = "interrupted"
	case early:
		state = "done (input stopped early)"
	}
	s := fmt.Sprintf("%s %.1f%% · %s files · %s · %s lines read · %s lines out · %.2fs",
		state, pct, commas(p.doneFiles.Load()), human(p.doneBytes.Load()),
		commas(p.lines.Load()), commas(p.outLines.Load()), time.Since(r.start).Seconds())
	if n := p.skipped.Load(); n > 0 {
		s += fmt.Sprintf(" · %s binary skipped", commas(n))
	}
	if n := p.errors.Load(); n > 0 {
		s += fmt.Sprintf(" · %d errors", n)
	}
	if total := p.idxTotal.Load(); total > 0 {
		s += fmt.Sprintf(" · index: read %s of %s", human(p.idxRead.Load()), human(total))
	}
	if redirect != "" {
		s += " → " + redirect
	}
	return s
}

func bar(pct float64) string {
	const w = 20
	n := int(pct / 100 * w)
	if n > w {
		n = w
	}
	b := strings.Repeat("=", n)
	if n < w {
		b += ">" + strings.Repeat(" ", w-n-1)
	}
	return "[" + b + "]"
}

func human(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	f := float64(n)
	for _, s := range []string{"KB", "MB", "GB", "TB"} {
		f /= u
		if f < u {
			return fmt.Sprintf("%.1f%s", f, s)
		}
	}
	return fmt.Sprintf("%.1fPB", f/u)
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// ---------------- ls ----------------

func listDir(args []word) ([]string, error) {
	p, err := getopt("ls", args, "la", "", nil)
	if err != nil {
		return nil, err
	}
	dir := "."
	if len(p.ops) > 0 {
		dir = p.ops[0].s
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	var out []string
	for _, e := range ents {
		name := e.Name()
		if !p.has('a') && strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		if p.has('l') {
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, fmt.Sprintf("%s %10s  %s  %s", info.Mode().String(), human(info.Size()),
				info.ModTime().Format("2006-01-02 15:04"), name))
		} else {
			out = append(out, name)
		}
	}
	return out, nil
}

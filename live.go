package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// Type-ahead mode: the command line is re-run on every keystroke and its results are
// drawn in a region below the prompt. Enter runs the line for real (full output in
// the scrollback), Esc exits psh.

const liveDebounce = 40 * time.Millisecond

const liveHint = " Esc: exit psh │ Enter: run with full output │ ↑/↓: history │ Ctrl+U: clear line "

type keyKind int

const (
	kRune keyKind = iota
	kEnter
	kBackspace
	kDelete
	kLeft
	kRight
	kHome
	kEnd
	kUp
	kDown
	kEsc
	kCtrlC
	kCtrlD
	kCtrlU
	kCtrlW
	kCtrlK
	kCtrlL
	kEOF
)

type keyEvent struct {
	kind keyKind
	r    rune
}

// parseKeys decodes raw terminal input. It returns the events and any trailing
// bytes that form an incomplete UTF-8 sequence.
func parseKeys(b []byte) ([]keyEvent, []byte) {
	var ev []keyEvent
	for len(b) > 0 {
		c := b[0]
		switch {
		case c == 0x1b:
			if len(b) >= 2 && (b[1] == '[' || b[1] == 'O') {
				j := 2
				for j < len(b) && !(b[j] >= 0x40 && b[j] <= 0x7e) {
					j++
				}
				if j >= len(b) {
					return ev, nil // truncated sequence: drop it
				}
				seq := string(b[2:j])
				switch b[j] {
				case 'A':
					ev = append(ev, keyEvent{kind: kUp})
				case 'B':
					ev = append(ev, keyEvent{kind: kDown})
				case 'C':
					ev = append(ev, keyEvent{kind: kRight})
				case 'D':
					ev = append(ev, keyEvent{kind: kLeft})
				case 'H':
					ev = append(ev, keyEvent{kind: kHome})
				case 'F':
					ev = append(ev, keyEvent{kind: kEnd})
				case '~':
					switch seq {
					case "1", "7":
						ev = append(ev, keyEvent{kind: kHome})
					case "4", "8":
						ev = append(ev, keyEvent{kind: kEnd})
					case "3":
						ev = append(ev, keyEvent{kind: kDelete})
					}
				}
				b = b[j+1:]
				continue
			}
			if len(b) >= 2 { // Alt+key: ignore the key
				_, n := utf8.DecodeRune(b[1:])
				b = b[1+n:]
				continue
			}
			ev = append(ev, keyEvent{kind: kEsc})
			b = b[1:]
		case c == '\r' || c == '\n':
			ev = append(ev, keyEvent{kind: kEnter})
			if c == '\r' && len(b) > 1 && b[1] == '\n' {
				b = b[1:]
			}
			b = b[1:]
		case c == 0x7f || c == 0x08:
			ev = append(ev, keyEvent{kind: kBackspace})
			b = b[1:]
		case c < 0x20:
			k := map[byte]keyKind{0x01: kHome, 0x05: kEnd, 0x02: kLeft, 0x06: kRight, 0x03: kCtrlC,
				0x04: kCtrlD, 0x15: kCtrlU, 0x17: kCtrlW, 0x0b: kCtrlK, 0x0c: kCtrlL, 0x10: kUp, 0x0e: kDown}
			if kind, ok := k[c]; ok {
				ev = append(ev, keyEvent{kind: kind})
			} else if c == '\t' {
				ev = append(ev, keyEvent{kind: kRune, r: ' '})
			}
			b = b[1:]
		default:
			if !utf8.FullRune(b) {
				return ev, append([]byte(nil), b...)
			}
			r, n := utf8.DecodeRune(b)
			ev = append(ev, keyEvent{kind: kRune, r: r})
			b = b[n:]
		}
	}
	return ev, nil
}

func readKeys(in io.Reader, ch chan<- keyEvent, sh *Shell) {
	buf := make([]byte, 1024)
	var rest []byte
	for {
		n, err := in.Read(buf)
		if n > 0 {
			ev, r := parseKeys(append(rest, buf[:n]...))
			rest = r
			for _, e := range ev {
				ch <- e
			}
		}
		if err != nil {
			if err == io.EOF && sh.recentSigint() { // Windows: Ctrl+C in cooked mode
				continue
			}
			ch <- keyEvent{kind: kEOF}
			return
		}
	}
}

// ---------------- editor ----------------

type liveEditor struct {
	sh   *Shell
	keys chan keyEvent

	buf     []rune
	cur     int
	hist    []string
	histPos int
	draft   []rune

	cancel context.CancelFunc

	ana analysis // guarded by mu

	mu        sync.Mutex
	gen       int
	status    string
	statusErr bool
	results   []string
	resCount  int
	preview   []string
	errs      []string
	done      bool
	dirty     bool
	drawnRows int // rows below the input line drawn by the last render
}

const maxKeptResults = 2000

func (sh *Shell) liveLoop(in io.Reader) error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	e := &liveEditor{sh: sh, keys: make(chan keyEvent, 256)}
	defer func() {
		e.clearRegion()
		term.Restore(fd, old)
	}()
	go readKeys(in, e.keys, sh)

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	e.dirty = true
	e.render()
	for {
		select {
		case k := <-e.keys:
			edited, quit, enter := e.handle(k)
			if quit {
				e.stopRun()
				return nil
			}
			if enter {
				e.stopRun()
				line := string(e.buf)
				e.clearRegion()
				os.Stdout.WriteString(e.sh.prompt() + line + "\r\n")
				term.Restore(fd, old)
				err := e.commit(line)
				if err == errExit {
					old = nil
					return nil
				}
				if old, err = term.MakeRaw(fd); err != nil {
					return err
				}
				e.buf, e.cur = nil, 0
				e.reanalyze()
				e.resetResults("")
				e.render()
				continue
			}
			if edited {
				e.stopRun()
				e.reanalyze()
				e.resetResults("")
				debounce.Reset(liveDebounce)
			}
			e.render()
		case <-debounce.C:
			e.startRun()
		case <-tick.C:
			e.render()
		}
	}
}

func (e *liveEditor) commit(line string) error {
	if s := strings.TrimSpace(line); s != "" {
		if len(e.hist) == 0 || e.hist[len(e.hist)-1] != line {
			e.hist = append(e.hist, line)
		}
	}
	e.histPos = len(e.hist)
	e.sh.dropJobCache()
	err := e.sh.runLine(line)
	if err != nil && err != errExit {
		fmt.Fprintln(os.Stderr, "psh:", err)
	}
	return err
}

// handle applies one key. It reports whether the line changed, psh should exit, or
// the line should be committed.
func (e *liveEditor) handle(k keyEvent) (edited, quit, enter bool) {
	switch k.kind {
	case kRune:
		e.buf = append(e.buf[:e.cur], append([]rune{k.r}, e.buf[e.cur:]...)...)
		e.cur++
		return true, false, false
	case kBackspace:
		if e.cur > 0 {
			e.buf = append(e.buf[:e.cur-1], e.buf[e.cur:]...)
			e.cur--
			return true, false, false
		}
	case kDelete:
		if e.cur < len(e.buf) {
			e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
			return true, false, false
		}
	case kLeft:
		e.cur = max(0, e.cur-1)
	case kRight:
		e.cur = min(len(e.buf), e.cur+1)
	case kHome:
		e.cur = 0
	case kEnd:
		e.cur = len(e.buf)
	case kCtrlU:
		e.buf, e.cur = e.buf[e.cur:], 0
		return true, false, false
	case kCtrlK:
		e.buf = e.buf[:e.cur]
		return true, false, false
	case kCtrlW:
		i := e.cur
		for i > 0 && e.buf[i-1] == ' ' {
			i--
		}
		for i > 0 && e.buf[i-1] != ' ' {
			i--
		}
		e.buf = append(e.buf[:i], e.buf[e.cur:]...)
		e.cur = i
		return true, false, false
	case kUp, kDown:
		if len(e.hist) == 0 {
			return
		}
		if e.histPos == len(e.hist) {
			e.draft = append([]rune(nil), e.buf...)
		}
		if k.kind == kUp {
			e.histPos = max(0, e.histPos-1)
		} else {
			e.histPos = min(len(e.hist), e.histPos+1)
		}
		if e.histPos == len(e.hist) {
			e.buf = append([]rune(nil), e.draft...)
		} else {
			e.buf = []rune(e.hist[e.histPos])
		}
		e.cur = len(e.buf)
		return true, false, false
	case kCtrlC:
		if e.cancel != nil {
			e.stopRun()
			e.mu.Lock()
			e.status, e.statusErr, e.dirty = "interrupted — Esc to exit", false, true
			e.mu.Unlock()
			return
		}
		e.buf, e.cur = nil, 0
		return true, false, false
	case kCtrlD:
		if len(e.buf) == 0 {
			return false, true, false
		}
	case kCtrlL:
		os.Stdout.WriteString("\x1b[2J\x1b[H")
		e.mu.Lock()
		e.drawnRows, e.dirty = 0, true
		e.mu.Unlock()
	case kEsc, kEOF:
		return false, true, false
	case kEnter:
		return false, false, true
	}
	e.mu.Lock()
	e.dirty = true
	e.mu.Unlock()
	return
}

func (e *liveEditor) stopRun() {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
}

func (e *liveEditor) resetResults(status string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gen++
	e.status, e.statusErr = status, false
	e.results, e.resCount, e.preview, e.errs, e.done = nil, 0, nil, nil, false
	e.dirty = true
	return e.gen
}

var builtinNames = map[string]bool{"exit": true, "quit": true, "cd": true, "pwd": true, "threads": true,
	"jobs": true, "clear": true, "cls": true, "help": true, "?": true, "version": true}

// analysis is the type-ahead view of the current line: the longest runnable prefix
// of the pipeline and where the incomplete part starts.
type analysis struct {
	ast     *pipelineAST // runnable leading commands (nil: nothing to run)
	badFrom int          // rune offset of the first incomplete command, -1 if none
	note    string
	noteErr bool
}

func (sh *Shell) analyze(line string) analysis {
	a := analysis{badFrom: -1}
	if strings.TrimSpace(line) == "" {
		return a
	}
	toks, errPos, lexErr := lexPos(line)
	for _, t := range toks {
		switch t.op {
		case ";":
			a.note = "several commands (;) are not previewed — press Enter to run"
			return a
		case ">", ">>":
			a.note = "redirection is not previewed — press Enter to run"
			return a
		}
	}
	type seg struct {
		cmd   rawCmd
		start int
		err   error
	}
	var segs []seg
	cur := seg{start: -1}
	for _, t := range toks {
		if t.op == "|" {
			if len(cur.cmd.args) == 0 {
				cur.start, cur.err = t.pos, errors.New("missing command before '|'")
			}
			segs = append(segs, cur)
			cur = seg{start: -1}
			continue
		}
		if cur.start < 0 {
			cur.start = t.pos
		}
		cur.cmd.args = append(cur.cmd.args, word{s: t.s, quoted: t.quoted, raw: t.raw, pos: t.pos})
	}
	switch {
	case lexErr != nil:
		if cur.start < 0 || len(cur.cmd.args) > 0 && exprCommands[cur.cmd.args[0].s] {
			cur.start = errPos // inside an expression: underline from the open quote
		}
		cur.err = lexErr
		segs = append(segs, cur)
	case len(cur.cmd.args) > 0:
		segs = append(segs, cur)
	case len(segs) > 0: // trailing '|'
		last := toks[len(toks)-1]
		segs = append(segs, seg{start: last.pos, err: errors.New("command expected after '|'")})
	}
	if len(segs) == 0 {
		return a
	}
	if len(segs) == 1 && segs[0].err == nil && builtinNames[segs[0].cmd.args[0].s] {
		a.note = "built-in command — press Enter to run"
		return a
	}
	valid := 0
	cmds := make([]rawCmd, 0, len(segs))
	for _, s := range segs {
		err := s.err
		if err == nil {
			_, err = sh.plan(&pipelineAST{cmds: append(cmds, s.cmd)}, nil)
		}
		if err != nil {
			a.badFrom, a.noteErr = s.start, true
			var pe *posError
			if errors.As(err, &pe) && pe.pos >= s.start {
				a.badFrom = pe.pos // underline only the bad part of an expression
			}
			a.note = "incomplete: " + err.Error()
			break
		}
		cmds = append(cmds, s.cmd)
		valid++
	}
	if valid > 0 {
		a.ast = &pipelineAST{cmds: cmds}
		if a.badFrom >= 0 {
			a.note += fmt.Sprintf(" — showing results of the first %d command(s)", valid)
		}
	}
	return a
}

func (e *liveEditor) reanalyze() {
	a := e.sh.analyze(string(e.buf))
	e.mu.Lock()
	e.ana = a
	e.dirty = true
	e.mu.Unlock()
}

func (e *liveEditor) startRun() {
	e.stopRun()
	e.mu.Lock()
	ast := e.ana.ast
	e.mu.Unlock()
	gen := e.resetResults("")
	if ast == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	run := &liveRun{e: e, gen: gen}
	go func() {
		err := e.sh.runPipeline(ctx, ast, run, true)
		if err != nil && ctx.Err() == nil {
			run.update(func() { e.status, e.statusErr, e.done = err.Error(), true, true })
		}
	}()
}

// ---------------- rendering ----------------

func (e *liveEditor) size() (int, int) {
	w, h := termSize(os.Stdout)
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	return w, h
}

// clearRegion erases the live region; the cursor is left at column 0 of the input line.
func (e *liveEditor) clearRegion() {
	os.Stdout.WriteString("\r\x1b[J")
}

func strWidth(rs []rune) int {
	n := 0
	for _, r := range rs {
		n += runeWidth(r)
	}
	return n
}

func (e *liveEditor) render() {
	e.mu.Lock()
	if !e.dirty {
		e.mu.Unlock()
		return
	}
	e.dirty = false
	w, h := e.size()
	maxRows := max(2, h-2) // rows below the input line

	var body []string
	for _, er := range e.errs {
		body = append(body, "\x00E"+er)
	}
	res := e.results
	if len(res) == 0 && !e.done {
		res = e.preview
	}
	room := maxRows - 3 - len(body) // keep note, status and hint rows
	if room < 1 {
		room = 1
	}
	if len(res) > room {
		body = append(body, res[:room-1]...)
		more := e.resCount - (room - 1)
		if len(e.results) == 0 {
			more = len(res) - (room - 1)
		}
		body = append(body, fmt.Sprintf("\x00D… %s more lines — press Enter for full output", commas(int64(more))))
	} else {
		body = append(body, res...)
	}
	status, statusErr := e.status, e.statusErr
	ana := e.ana
	e.mu.Unlock()

	var b strings.Builder
	b.WriteString("\x1b[?2026h\x1b[?25l\r")
	// input line with horizontal scrolling; the incomplete part is underlined in red
	prompt, pw := e.sh.livePrompt(w)
	avail := max(1, w-1-pw)
	start := 0
	for strWidth(e.buf[start:e.cur]) > avail-1 {
		start++
	}
	b.WriteString(prompt)
	cols, marked := 0, false
	for i := start; i < len(e.buf); i++ {
		r := e.buf[i]
		if r < 32 {
			r = '?'
		}
		if cols+runeWidth(r) > avail {
			break
		}
		if !marked && ana.badFrom >= 0 && i >= ana.badFrom {
			b.WriteString("\x1b[4;31m")
			marked = true
		}
		b.WriteRune(r)
		cols += runeWidth(r)
	}
	b.WriteString("\x1b[0m\x1b[K")
	cursorCol := pw + strWidth(e.buf[start:e.cur])

	rows := 0
	line := func(s, style string) {
		b.WriteString("\r\n" + style + truncateWidth(s, w-1) + "\x1b[0m\x1b[K")
		rows++
	}
	if ana.note != "" {
		if ana.noteErr {
			line(ana.note, "\x1b[33m")
		} else {
			line(ana.note, "\x1b[2m")
		}
	}
	if statusErr {
		line(status, "\x1b[31m")
	} else {
		line(status, "\x1b[36m")
	}
	for _, l := range body {
		switch {
		case strings.HasPrefix(l, "\x00E"):
			line(l[2:], "\x1b[31m")
		case strings.HasPrefix(l, "\x00D"):
			line(l[2:], "\x1b[2m")
		case strings.HasPrefix(l, "── live preview"):
			line(l, "\x1b[2m")
		default:
			line(l, "")
		}
	}
	line(liveHint, "\x1b[7m")
	b.WriteString("\x1b[J") // remove leftovers of a taller previous frame
	fmt.Fprintf(&b, "\x1b[%dA\r", rows)
	if cursorCol > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", cursorCol)
	}
	b.WriteString("\x1b[?25h\x1b[?2026l")
	os.Stdout.WriteString(b.String())
	e.mu.Lock()
	e.drawnRows = rows
	e.mu.Unlock()
}

// ---------------- liveRun: display for one preview run ----------------

type liveRun struct {
	e   *liveEditor
	gen int
}

func (r *liveRun) update(f func()) {
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	if r.e.gen != r.gen {
		return // a newer keystroke superseded this run
	}
	f()
	r.e.dirty = true
}

func (r *liveRun) Begin()             {}
func (r *liveRun) Live() bool         { return true }
func (r *liveRun) SetStatus(s string) { r.update(func() { r.e.status, r.e.statusErr = s, false }) }
func (r *liveRun) SetLive(lines []string) {
	r.update(func() { r.e.preview = lines })
}

func (r *liveRun) PreviewRows() int {
	_, h := r.e.size()
	return max(3, h-6)
}

func (r *liveRun) Print(lines []string) {
	r.update(func() {
		e := r.e
		e.resCount += len(lines)
		if n := maxKeptResults - len(e.results); n > 0 {
			e.results = append(e.results, lines[:min(n, len(lines))]...)
		}
	})
}

func (r *liveRun) Errorf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	r.update(func() {
		if len(r.e.errs) < 3 {
			r.e.errs = append(r.e.errs, msg)
		}
	})
}

func (r *liveRun) End(summary string) {
	r.update(func() {
		if summary != "" {
			r.e.status, r.e.statusErr = summary, false
		}
		r.e.done = true
		r.e.preview = nil
	})
}

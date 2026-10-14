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
// the scrollback), Esc exits plumb.

const liveDebounce = 40 * time.Millisecond

const (
	liveHint   = " Esc: exit plumb │ Tab: SPL/shell │ Enter: run with full output │ ↑/↓: browse results │ Ctrl+P/N: history "
	listHint   = " ↑/↓ PgUp/PgDn: move │ Enter: details │ Esc: back to input │ type to edit "
	detailHint = " ↑/↓: previous/next result │ PgUp/PgDn: scroll │ Esc/Enter: back to list "
)

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
	kPgUp
	kPgDn
	kHistPrev // Ctrl+P
	kHistNext // Ctrl+N
	kTab      // switch between SPL and shell mode
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
					case "5":
						ev = append(ev, keyEvent{kind: kPgUp})
					case "6":
						ev = append(ev, keyEvent{kind: kPgDn})
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
				0x04: kCtrlD, 0x15: kCtrlU, 0x17: kCtrlW, 0x0b: kCtrlK, 0x0c: kCtrlL, 0x10: kHistPrev, 0x0e: kHistNext}
			if kind, ok := k[c]; ok {
				ev = append(ev, keyEvent{kind: kind})
			} else if c == '\t' {
				ev = append(ev, keyEvent{kind: kTab})
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
	rows      *rowLog // results of the current run
	preview   []string
	errs      []string
	done      bool
	dirty     bool
	drawnRows int // rows below the input line drawn by the last render

	// browsing results (guarded by mu)
	mode         editMode
	sel, top     int      // selected result, first result shown
	detail       []string // detail view of the selected result
	detailScroll int
	timeline     *timeline // of the current run (event searches)
	lastW, lastH int       // terminal size of the last frame
	srcCol       bool      // events: file names as a column
}

type editMode int

const (
	modeEdit   editMode = iota // typing the command line
	modeList                   // browsing results with the arrow keys
	modeDetail                 // showing one result in full
)

func (sh *Shell) liveLoop(in io.Reader) error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	e := &liveEditor{sh: sh, keys: make(chan keyEvent, 256)}
	defer func() {
		e.stopRun()
		e.mu.Lock()
		if e.rows != nil {
			e.rows.close()
		}
		e.mu.Unlock()
		e.clearRegion()
		if old != nil {
			term.Restore(fd, old)
		}
	}()
	go readKeys(in, e.keys, sh)

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	var lastStatus *storeStatus
	e.reanalyze()
	e.startRun() // the empty line shows the latest events
	e.render()
	for {
		select {
		case k := <-e.keys:
			edited, quit, enter := e.handle(k)
			if quit {
				e.stopRun()
				e.clearRegion()
				e.askDeleteIndex()
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
					if raw, err := term.MakeRaw(fd); err == nil { // one key for the question
						e.askDeleteIndex()
						term.Restore(fd, raw)
					}
					old = nil
					return nil
				}
				if old, err = term.MakeRaw(fd); err != nil {
					return err
				}
				e.buf, e.cur = nil, 0
				e.mu.Lock()
				e.lastW, e.lastH = e.size() // a resize during the run: keep its output on screen
				e.mu.Unlock()
				e.reanalyze()
				e.startRun()
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
			e.checkResize()
			if e.sh.store != nil {
				if st := e.sh.store.status.Load(); st != lastStatus {
					lastStatus = st
					e.mu.Lock()
					e.dirty = true
					e.mu.Unlock()
				}
			}
			e.render()
		}
	}
}

// checkResize redraws the whole screen when the terminal size changed: lines
// drawn for the old width may have wrapped, so the region can't be redrawn in
// place. (Polling the size works on every platform, Windows included.)
func (e *liveEditor) checkResize() {
	w, h := e.size()
	e.mu.Lock()
	defer e.mu.Unlock()
	if w == e.lastW && h == e.lastH {
		return
	}
	first := e.lastW == 0
	e.lastW, e.lastH = w, h
	if first {
		return
	}
	os.Stdout.WriteString("\x1b[2J\x1b[H")
	e.drawnRows, e.dirty = 0, true
	if e.mode == modeDetail {
		scroll := e.detailScroll
		e.buildDetail() // wrapped for the new width
		e.detailScroll = min(scroll, max(0, len(e.detail)-1))
	}
}

// askDeleteIndex asks on exit whether to delete the persistent index. The key
// reader owns stdin, so the answer is one key from e.keys: y deletes, anything
// else keeps it.
func (e *liveEditor) askDeleteIndex() {
	st := e.sh.store
	if st == nil || !st.persistent {
		return
	}
	os.Stdout.WriteString("\r" + deleteIndexQuestion(st))
	k, ok := <-e.keys
	del := ok && k.kind == kRune && (k.r == 'y' || k.r == 'Y')
	answer := "n"
	if del {
		answer = "y"
	}
	os.Stdout.WriteString(answer + "\r\n")
	e.sh.indexAsked, e.sh.indexDelete = true, del
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
		fmt.Fprintln(os.Stderr, "plumb:", err)
	}
	return err
}

// handle applies one key. It reports whether the line changed, plumb should exit, or
// the line should be committed.
func (e *liveEditor) handle(k keyEvent) (edited, quit, enter bool) {
	e.mu.Lock()
	mode := e.mode
	e.mu.Unlock()
	if mode != modeEdit && e.browse(k, mode) {
		return false, false, false
	}
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
	case kUp, kDown, kPgUp, kPgDn:
		if e.startBrowsing(k.kind) {
			return
		}
		if k.kind == kPgUp || k.kind == kPgDn {
			return
		}
		fallthrough // no results: arrows browse the history
	case kHistPrev, kHistNext:
		if len(e.hist) == 0 {
			return
		}
		if e.histPos == len(e.hist) {
			e.draft = append([]rune(nil), e.buf...)
		}
		if k.kind == kUp || k.kind == kHistPrev {
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
	case kTab:
		if e.sh.lang == langSPL {
			e.sh.lang = langShell
		} else {
			e.sh.lang = langSPL
		}
		e.sh.dropJobCache()
		return true, false, false
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
	if e.rows != nil {
		e.rows.close()
	}
	dir := ""
	if e.sh.store != nil {
		dir = e.sh.store.dir
	}
	e.rows = &rowLog{dir: dir}
	e.preview, e.errs, e.done, e.timeline = nil, nil, false, nil
	e.mode, e.sel, e.top, e.detail = modeEdit, 0, 0, nil
	e.dirty = true
	return e.gen
}

var builtinNames = map[string]bool{"exit": true, "quit": true, "spl": true, "sh": true, "config": true, "cd": true, "pwd": true, "threads": true,
	"jobs": true, "clear": true, "cls": true, "help": true, "?": true, "version": true, "index": true}

// analysis is the type-ahead view of the current line: the longest runnable prefix
// of the pipeline and where the incomplete part starts.
type analysis struct {
	ast      *pipelineAST // runnable leading commands (nil: nothing to run)
	badFrom  int          // rune offset of the first incomplete command, -1 if none
	note     string
	noteErr  bool
	latest   bool // the line is empty: ast shows the latest events
	expanded bool // macros were expanded: badFrom is not a position in the line
}

// latestQuery (SPL) is previewed while the command line is empty: the newest
// events first.
const latestQuery = "_time=*"

// isBuiltinLine reports whether line is a single built-in command (cd, help, ...),
// which works the same in both modes.
func isBuiltinLine(line string) bool {
	toks, err := lex(line)
	if err != nil || len(toks) == 0 || toks[0].op != "" || !builtinNames[toks[0].s] {
		return false
	}
	for _, t := range toks {
		if t.op != "" {
			return false
		}
	}
	return true
}

func (sh *Shell) analyze(line string) analysis {
	a := analysis{badFrom: -1}
	if strings.TrimSpace(line) == "" {
		if ast, err := splParse(latestQuery); err == nil {
			a.ast, a.latest = ast, true
			a.note = "latest events — type a query to search"
		}
		return a
	}
	if isBuiltinLine(line) {
		a.note = "built-in command — press Enter to run"
		return a
	}
	type seg struct {
		cmd   rawCmd
		start int
		err   error
	}
	var segs []seg
	spl := sh.lang == langSPL
	notPreviewed := ""
	if spl {
		if q, err := expandQuery(line); err != nil {
			a.note, a.noteErr, a.badFrom = "incomplete: "+err.Error(), true, 0
			return a
		} else if q != line {
			line = q // macros and saved searches: positions refer to the expanded query
			a.expanded = true
		}
		for _, s := range splitSPL(line) {
			if s.err == nil && s.cmd.args[0].s == "outputcsv" {
				notPreviewed = "outputcsv is not previewed — press Enter to run"
				break
			}
			segs = append(segs, seg{cmd: s.cmd, start: s.start, err: s.err})
		}
	} else {
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
			cur.cmd.args = append(cur.cmd.args, word{s: t.s, quoted: t.quoted, pos: t.pos})
		}
		switch {
		case lexErr != nil:
			if cur.start < 0 {
				cur.start = errPos
			}
			cur.err = lexErr
			segs = append(segs, cur)
		case len(cur.cmd.args) > 0:
			segs = append(segs, cur)
		case len(segs) > 0: // trailing '|'
			last := toks[len(toks)-1]
			segs = append(segs, seg{start: last.pos, err: errors.New("command expected after '|'")})
		}
	}
	if len(segs) == 0 {
		a.note = notPreviewed
		return a
	}
	valid := 0
	cmds := make([]rawCmd, 0, len(segs))
	for _, s := range segs {
		err := s.err
		if err == nil {
			_, err = sh.plan(&pipelineAST{cmds: append(cmds, s.cmd), spl: spl}, nil)
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
	if a.expanded && a.badFrom >= 0 {
		a.badFrom = len([]rune(line)) // underline nothing rather than the wrong part
	}
	if valid > 0 {
		a.ast = &pipelineAST{cmds: cmds, spl: spl}
		if a.badFrom >= 0 {
			a.note += fmt.Sprintf(" — showing results of the first %d command(s)", valid)
		}
	}
	if a.note == "" {
		a.note = notPreviewed
	}
	if a.note == "" && spl {
		// "stats count" as a whole query searches for the words "stats" and "count"
		if f := strings.Fields(line); splCommands[f[0]] && f[0] != "search" {
			a.note = fmt.Sprintf("searching for the word %q — start with * | %s ... to use the command on all events", f[0], f[0])
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
	if e.mode == modeEdit {
		for _, er := range e.errs {
			body = append(body, "\x00E"+er)
		}
	}
	tl := ""
	if e.mode == modeEdit {
		tl = e.timeline.render(w)
	}
	room := max(1, maxRows-3-len(body)) // keep note, status and hint rows
	if tl != "" {
		room = max(1, room-1)
	}
	body = append(body, e.bodyLines(room)...)
	status, statusErr := e.status, e.statusErr
	ana := e.ana
	hint := map[editMode]string{modeEdit: liveHint, modeList: listHint, modeDetail: detailHint}[e.mode]
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
	if tl != "" {
		if colorMode == colorNever {
			line(tl, "")
		} else {
			line(tl, "\x1b[32m")
		}
	}
	for _, l := range body {
		switch {
		case strings.HasPrefix(l, "\x00E"):
			line(l[2:], "\x1b[31m")
		case strings.HasPrefix(l, "\x00D"):
			line(l[2:], "\x1b[2m")
		case strings.HasPrefix(l, "\x00S"): // selected result
			line(l[2:], "\x1b[7m")
		case strings.HasPrefix(l, "\x00H"): // detail heading
			line(l[2:], "\x1b[1m")
		case strings.HasPrefix(l, "── live preview"):
			line(l, "\x1b[2m")
		default:
			line(l, "")
		}
	}
	if e.sh.store != nil {
		if t := e.sh.store.statusText(); t != "" {
			hint += "│ " + t + " "
		}
	}
	line(hint, "\x1b[7m")
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

func (r *liveRun) SetTimeline(t *timeline)    { r.update(func() { r.e.timeline = t }) }
func (r *liveRun) SetColumns(src bool, _ int) { r.update(func() { r.e.srcCol = src }) }

func (r *liveRun) PreviewRows() int {
	_, h := r.e.size()
	return max(3, h-6)
}

func (r *liveRun) Print(lines []Line) {
	r.update(func() {
		for _, l := range lines {
			r.e.rows.add(l)
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

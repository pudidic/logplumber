package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Browsing results in type-ahead mode: the arrow keys move from the command line
// into the full result list, Enter shows one result in detail.

const contextRadius = 5

// startBrowsing switches to the result list if there are results.
func (e *liveEditor) startBrowsing(k keyKind) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rows == nil || e.rows.len() == 0 {
		return false
	}
	e.mode = modeList
	e.sel, e.top = 0, 0
	if k == kUp {
		e.sel = e.rows.len() - 1
	}
	e.dirty = true
	return true
}

// browse handles a key in list or detail mode. It returns false for keys that go
// back to editing the command line (they are then applied there).
func (e *liveEditor) browse(k keyEvent, mode editMode) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dirty = true
	n := e.rows.len()
	page := max(1, e.bodyRoom()-2)
	move := func(d int) {
		e.sel = max(0, min(n-1, e.sel+d))
	}
	if mode == modeDetail {
		switch k.kind {
		case kUp, kDown:
			if k.kind == kUp {
				move(-1)
			} else {
				move(1)
			}
			e.buildDetail()
		case kPgUp:
			e.detailScroll = max(0, e.detailScroll-page)
		case kPgDn:
			e.detailScroll = max(0, min(len(e.detail)-1, e.detailScroll+page))
		case kEsc, kEnter, kBackspace:
			e.mode, e.detail = modeList, nil
		case kCtrlC:
			e.mode, e.detail = modeEdit, nil
		default:
			e.mode, e.detail = modeEdit, nil
			return false
		}
		return true
	}
	switch k.kind {
	case kUp:
		move(-1)
	case kDown:
		move(1)
	case kPgUp:
		move(-page)
	case kPgDn:
		move(page)
	case kHome:
		e.sel = 0
	case kEnd:
		e.sel = max(0, n-1)
	case kEnter:
		e.mode = modeDetail
		e.buildDetail()
	case kEsc, kCtrlC:
		e.mode = modeEdit
	default:
		e.mode = modeEdit
		return false
	}
	return true
}

// bodyLines returns what is shown below the status line: the detail view, the
// result list with the selection, or the first results and the live preview.
// Called with mu held.
func (e *liveEditor) bodyLines(room int) []string {
	if e.mode == modeDetail {
		end := min(len(e.detail), e.detailScroll+room)
		return e.detail[min(e.detailScroll, end):end]
	}
	n := 0
	if e.rows != nil {
		n = e.rows.len()
	}
	if n == 0 {
		if e.done {
			return nil
		}
		return e.preview[:min(len(e.preview), room)]
	}
	var out []string
	if h := e.rows.header; h != nil && room > 3 {
		out = append(out, "\x00D"+h.text, "\x00D"+h.rule)
		room -= 2
	}
	if e.mode == modeList {
		if e.sel < e.top {
			e.top = e.sel
		}
		if e.sel >= e.top+room {
			e.top = e.sel - room + 1
		}
		for i := e.top; i < n && i < e.top+room; i++ {
			l, _ := e.rows.get(i)
			if i == e.sel {
				out = append(out, "\x00S"+l.Text)
			} else {
				out = append(out, l.Text)
			}
		}
		return out
	}
	show := n
	if n > room {
		show = room - 1
	}
	for i := 0; i < show; i++ {
		l, _ := e.rows.get(i)
		out = append(out, l.Text)
	}
	if n > show {
		out = append(out, fmt.Sprintf("\x00D… %s more lines — ↓ to browse all results, Enter for full output", commas(int64(n-show))))
	}
	return out
}

// bodyRoom is the number of result rows that fit on the screen.
func (e *liveEditor) bodyRoom() int {
	_, h := e.size()
	return max(1, h-2-3)
}

// buildDetail renders the selected result: source, full text, fields and the
// surrounding lines of the source file. Called with mu held.
func (e *liveEditor) buildDetail() {
	w, _ := e.size()
	width := max(20, w-3)
	l, ok := e.rows.get(e.sel)
	e.detailScroll = 0
	if !ok {
		e.detail = []string{"(result no longer available)"}
		return
	}
	var out []string
	out = append(out, fmt.Sprintf("\x00Hresult %s of %s", commas(int64(e.sel+1)), commas(int64(e.rows.len()))))
	if l.Src != "" {
		loc := l.Src
		if l.No > 0 {
			loc = fmt.Sprintf("%s:%d", l.Src, l.No)
		}
		out = append(out, "source: "+loc)
	}
	out = append(out, "", "\x00Htext")
	for _, s := range wrapWidth(l.Text, width) {
		out = append(out, "  "+s)
	}
	if names := l.FieldNames(); len(names) > 0 {
		sort.Strings(names)
		out = append(out, "", "\x00Hfields")
		for _, n := range names {
			v, _ := l.Field(n)
			out = append(out, fmt.Sprintf("  %s = %s", n, v))
		}
	}
	if l.Src != "" && l.No > 0 {
		lines, first, err := e.sh.contextLines(l.Src, l.No, contextRadius)
		out = append(out, "", "\x00Hcontext")
		if err != nil {
			out = append(out, "  ("+err.Error()+")")
		}
		for i, s := range lines {
			no := first + i
			mark := " "
			if no == l.No {
				mark = ">"
			}
			out = append(out, fmt.Sprintf("%s%6d  %s", mark, no, s))
		}
	}
	e.detail = out
}

// wrapWidth splits s into pieces of at most w terminal columns.
func wrapWidth(s string, w int) []string {
	var out []string
	var cur strings.Builder
	cols := 0
	for _, r := range s {
		if r == '\t' {
			r = ' '
		}
		rw := runeWidth(r)
		if cols+rw > w {
			out = append(out, cur.String())
			cur.Reset()
			cols = 0
		}
		cur.WriteRune(r)
		cols += rw
	}
	return append(out, cur.String())
}

// contextLines returns the lines around line no of the source shown as disp
// ("path" or "path::member"), and the number of the first returned line.
func (sh *Shell) contextLines(disp string, no, radius int) ([]string, int, error) {
	file, member := disp, ""
	if i := strings.Index(disp, "::"); i >= 0 {
		file, member = disp[:i], disp[i:]
	}
	from, to := max(1, no-radius), no+radius
	collect := func(r io.Reader, line int) ([]string, int, error) {
		br := bufio.NewReaderSize(r, 64<<10)
		var out []string
		first := 0
		for ; line <= to; line++ {
			s, err := br.ReadString('\n')
			if s == "" && err != nil {
				break
			}
			if line >= from {
				if first == 0 {
					first = line
				}
				out = append(out, strings.TrimRight(s, "\r\n"))
			}
			if err != nil {
				break
			}
		}
		return out, first, nil
	}
	if u, chunks, ok := sh.storeUnit(file, member); ok {
		f, err := os.Open(u.path)
		if err != nil {
			return nil, 0, err
		}
		defer f.Close()
		// Start at the last chunk that begins at or before the first wanted line.
		var off int64
		line := 1
		if i := sort.Search(len(chunks), func(i int) bool { return chunks[i].line > from }); i > 0 {
			off, line = chunks[i-1].off, chunks[i-1].line
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, 0, err
		}
		return collect(f, line)
	}
	// Without the store: unpack the file until the member is found.
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	var lines []string
	var first int
	found := false
	a := &archiveWalker{ctx: context.Background(), disp: file, onErr: func(error) {},
		leaf: func(m, _ string, br *bufio.Reader, _ bool) error {
			if m != member {
				return nil
			}
			found = true
			lines, first, _ = collect(br, 1)
			return errStop
		}}
	if err := a.walkFile(f, fi.Size(), f); err != nil && err != errStop {
		return nil, 0, err
	}
	if !found {
		return nil, 0, fmt.Errorf("%s not found", disp)
	}
	return lines, first, nil
}

// storeUnit finds a source in the index snapshot, with its chunks.
func (sh *Shell) storeUnit(file, member string) (*unit, []chunkRec, bool) {
	if sh.store == nil {
		return nil, nil, false
	}
	snap := sh.store.snap.Load()
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, nil, false
	}
	e := snap.srcs[abs]
	if e == nil {
		return nil, nil, false
	}
	if fi, err := os.Stat(file); err != nil || !e.valid(fi) {
		return nil, nil, false
	}
	for _, u := range e.units {
		if u.member == member && !u.binary {
			lo, hi := int(u.firstChunk), int(u.firstChunk+u.nChunks)
			if hi > len(snap.chunks) {
				return u, nil, true
			}
			return u, snap.chunks[lo:hi], true
		}
	}
	return nil, nil, false
}

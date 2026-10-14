package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// UI owns the terminal. Results scroll normally; below them a "live region"
// (preview lines + one status line) is redrawn in place with ANSI escapes.
type UI struct {
	mu     sync.Mutex
	out    *bufio.Writer
	active bool // stdout is an ANSI-capable terminal
	status string
	live   []string
	drawn  int // lines currently occupied by the live region
	inRun  bool
	header *tableHeader // last table header printed

	timeline  *timeline // event search: drawn in the live region, then above the results
	tlPrinted bool
}

// SetTimeline sets the timeline of the current run (nil: none).
func (u *UI) SetTimeline(t *timeline) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.timeline, u.tlPrinted = t, false
}

const timelineStyle = "\x1b[32m"

func newUI() *UI {
	u := &UI{out: bufio.NewWriterSize(os.Stdout, 256<<10)}
	if isTerminal(os.Stdout) && enableVT(os.Stdout) {
		u.active = true
	}
	return u
}

func (u *UI) size() (int, int) {
	w, h := termSize(os.Stdout)
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	return w, h
}

func (u *UI) Live() bool { return u.active }

func (u *UI) PreviewRows() int {
	_, h := u.size()
	return min(20, h/2)
}

func (u *UI) clearRegion() {
	if u.drawn == 0 {
		return
	}
	if u.drawn > 1 {
		fmt.Fprintf(u.out, "\x1b[%dA", u.drawn-1)
	}
	u.out.WriteString("\r\x1b[J")
	u.drawn = 0
}

func (u *UI) drawRegion() {
	if !u.active || !u.inRun {
		return
	}
	w, h := u.size()
	lines := u.live
	if len(lines) > h-2 {
		lines = lines[:max(0, h-2)]
	}
	for i, l := range lines {
		t := truncateWidth(l, w-1)
		if i == 0 {
			u.out.WriteString("\x1b[2m" + t + "\x1b[0m\n")
		} else {
			u.out.WriteString(t + "\n")
		}
	}
	u.drawn = len(lines) + 1
	if !u.tlPrinted {
		if tl := u.timeline.render(w); tl != "" {
			u.out.WriteString(timelineStyle + truncateWidth(tl, w-1) + "\x1b[0m\n")
			u.drawn++
		}
	}
	u.out.WriteString("\x1b[36m" + truncateWidth(u.status, w-1) + "\x1b[0m")
}

// Begin starts a pipeline run.
func (u *UI) Begin() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.inRun, u.status, u.live, u.header = true, "", nil, nil
	if u.active {
		u.out.WriteString("\x1b[?25l")
		u.out.Flush()
	}
}

func (u *UI) SetStatus(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = s
	if !u.active {
		return
	}
	u.clearRegion()
	u.drawRegion()
	u.out.Flush()
}

func (u *UI) SetLive(lines []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.live = lines
	if !u.active {
		return
	}
	u.clearRegion()
	u.drawRegion()
	u.out.Flush()
}

// Print writes result lines above the live region.
func (u *UI) Print(lines []Line) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearRegion()
	if u.active && !u.tlPrinted && len(lines) > 0 {
		// the results of an event search come after all input: the timeline is complete
		u.tlPrinted = true
		w, _ := u.size()
		if tl := u.timeline.render(w); tl != "" {
			u.out.WriteString(timelineStyle + truncateWidth(tl, w-1) + "\x1b[0m\n")
		}
	}
	for _, l := range lines {
		if h := lineHeader(l); h != nil && h != u.header {
			u.header = h
			u.out.WriteString(h.text + "\n" + h.rule + "\n")
		}
		u.out.WriteString(l.Text)
		u.out.WriteByte('\n')
	}
	u.drawRegion()
	u.out.Flush()
}

func (u *UI) Errorf(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearRegion()
	u.out.Flush()
	msg := fmt.Sprintf(format, a...)
	if u.active {
		msg = "\x1b[31m" + msg + "\x1b[0m"
	}
	fmt.Fprintln(os.Stderr, msg)
	u.drawRegion()
	u.out.Flush()
}

// End removes the live region and prints the final summary line.
func (u *UI) End(summary string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearRegion()
	u.inRun = false
	u.live = nil
	if u.active {
		if summary != "" {
			u.out.WriteString("\x1b[2m" + summary + "\x1b[0m\n")
		}
		u.out.WriteString("\x1b[?25h")
	}
	u.out.Flush()
}

func runeWidth(r rune) int {
	if r < 0x1100 {
		return 1
	}
	if r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1f64f) ||
		(r >= 0x1f900 && r <= 0x1f9ff) ||
		(r >= 0x20000 && r <= 0x3fffd) {
		return 2
	}
	return 1
}

// truncateWidth cuts s to at most w terminal columns and removes control characters
// so the live region never wraps (which would break cursor movement).
func truncateWidth(s string, w int) string {
	var b strings.Builder
	cols := 0
	for _, r := range s {
		if r == '\t' {
			r = ' '
		} else if r < 32 || r == 0x7f {
			r = '?'
		}
		rw := runeWidth(r)
		if cols+rw > w {
			break
		}
		cols += rw
		b.WriteRune(r)
	}
	return b.String()
}

package main

import (
	"strconv"
	"strings"
)

// Source columns
//
// In a terminal, result lines that come with a file name are shown in two
// aligned columns, the file (magenta, with the line number in green, as GNU
// grep colors them) and the content:
//
//	apache/error.log:12   [Wed Oct 14 09:01:48 2026] [core:error] ...
//	app/app.jsonl         {"ts":"2026-10-14T09:00:00.555+09:00", ...}
//
// This applies to the events of an SPL search and to grep's "file:" and
// "file:line:" prefixes. Output to a file or a pipe keeps the plain text.
//
// Colors are written as markers ("\x00K35;") that become ANSI codes only when
// drawn, so escape sequences inside log text are still neutralized.

const maxSrcColumn = 40 // wider file names are shortened at the front

const (
	colorFile = 35 // magenta
	colorNum  = 32 // green
	colorSep  = 36 // cyan
	colorOff  = 39 // default foreground (keeps reverse video of a selection)
)

func colorMark(code int) string { return "\x00K" + strconv.Itoa(code) + ";" }

// lineColumns splits a result line into its file label parts and the content.
func lineColumns(l Line, srcCol bool) (file, num, rest string, ok bool) {
	if lineHeader(l) != nil {
		return "", "", "", false
	}
	if l.Pre > 0 && l.Pre <= len(l.Text) && l.PreName <= l.Pre {
		file = strings.TrimSuffix(l.Text[:l.PreName], ":")
		num = strings.TrimSuffix(l.Text[l.PreName:l.Pre], ":")
		return file, num, l.Text[l.Pre:], true
	}
	if srcCol && l.Src != "" {
		return l.Src, "", l.Text, true
	}
	return "", "", "", false
}

// labelWidth is the display width of a file label.
func labelWidth(file, num string) int {
	w := strWidth([]rune(file))
	if num != "" {
		w += 1 + len(num)
	}
	return min(w, maxSrcColumn)
}

// columnWidth is the label column width for a set of lines.
func columnWidth(lines []Line, srcCol bool) int {
	w := 0
	for _, l := range lines {
		if f, n, _, ok := lineColumns(l, srcCol); ok {
			w = max(w, labelWidth(f, n))
		}
	}
	return w
}

// displayLine renders a result line for the terminal: the two columns with
// color markers when it has a file label, otherwise its text.
func displayLine(l Line, srcCol bool, width int) string {
	file, num, rest, ok := lineColumns(l, srcCol)
	if !ok {
		return l.Text
	}
	if room := maxSrcColumn - labelWidth("", num); strWidth([]rune(file)) > room {
		rs := []rune(file)
		file = "…" + string(rs[len(rs)-(room-1):]) // keep the end: the name
	}
	var b strings.Builder
	b.WriteString(colorMark(colorFile) + file + colorMark(colorOff))
	if num != "" {
		b.WriteString(colorMark(colorSep) + ":" + colorMark(colorNum) + num + colorMark(colorOff))
	}
	b.WriteString(strings.Repeat(" ", max(0, width-labelWidth(file, num))+2))
	b.WriteString(rest)
	return b.String()
}

// colorMarker reads a "\x00K<code>;" marker at s[i:]; n is its length.
func colorMarker(s string, i int) (code string, n int) {
	if i+3 >= len(s) || s[i] != 0 || s[i+1] != 'K' {
		return "", 0
	}
	j := i + 2
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == i+2 || j >= len(s) || s[j] != ';' {
		return "", 0
	}
	return s[i+2 : j], j + 1 - i
}

// ansiColors turns color markers into ANSI codes (for output that is not
// truncated, such as the full output of Enter).
func ansiColors(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if code, n := colorMarker(s, i); n > 0 {
			b.WriteString("\x1b[" + code + "m")
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// stripColors removes color markers (for widths and plain text).
func stripColors(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if _, n := colorMarker(s, i); n > 0 {
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

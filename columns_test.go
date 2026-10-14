package main

import (
	"bufio"
	"strings"
	"testing"
)

func TestGrepPrefixFields(t *testing.T) {
	toks, _ := lex("grep -Hn err")
	ast, _ := parsePipeline(toks)
	s, _, _ := buildSpec(ast.cmds[0], true)
	s.Setup(planCtx{first: true, multi: true}, s)
	g := s.New()
	var got Line
	g.Push(Line{Text: "an error here", Src: "dir/app.log", No: 12}, func(l Line) bool { got = l; return true })
	if got.Text != "dir/app.log:12:an error here" || got.Text[:got.PreName] != "dir/app.log:" || got.Text[:got.Pre] != "dir/app.log:12:" {
		t.Errorf("grep -Hn: %q pre=%d name=%d", got.Text, got.Pre, got.PreName)
	}
}

func TestDisplayColumns(t *testing.T) {
	lines := []Line{
		{Text: "a.log:3:first", Src: "a.log", Pre: 8, PreName: 6},
		{Text: "longer/name.log:12:second", Src: "longer/name.log", Pre: 19, PreName: 16},
	}
	w := columnWidth(lines, false)
	if w != len("longer/name.log:12") {
		t.Errorf("width %d", w)
	}
	plain := []string{}
	for _, l := range lines {
		plain = append(plain, stripColors(displayLine(l, false, w)))
	}
	want := []string{"a.log:3" + strings.Repeat(" ", 18-7+2) + "first", "longer/name.log:12  second"}
	if strings.Join(plain, "|") != strings.Join(want, "|") {
		t.Errorf("columns:\n%q\n%q", plain, want)
	}
	// SPL events: the file from Src
	ev := Line{Text: "event text", Src: "x/y.log"}
	if got := stripColors(displayLine(ev, true, 7)); got != "x/y.log  event text" {
		t.Errorf("event: %q", got)
	}
	if got := displayLine(ev, false, 7); got != "event text" {
		t.Errorf("without source column: %q", got)
	}
	// table rows are left alone
	var row Line
	makeTable([]string{"a"}, [][]string{{"1"}}, func(l Line) bool { row = l; return true })
	if got := displayLine(Line{Text: row.Text, Rec: row.Rec, Src: "f"}, true, 5); got != row.Text {
		t.Errorf("table row: %q", got)
	}
	// long names keep their end
	long := Line{Text: "x", Src: strings.Repeat("d/", 30) + "app.log"}
	if got := stripColors(displayLine(long, true, maxSrcColumn)); !strings.HasPrefix(got, "…") || !strings.Contains(got, "app.log  x") {
		t.Errorf("long name: %q", got)
	}
	// colors
	c := displayLine(lines[0], false, w)
	if a := ansiColors(c); !strings.Contains(a, "\x1b[35ma.log\x1b[39m") || !strings.Contains(a, "\x1b[32m3\x1b[39m") {
		t.Errorf("ansi: %q", a)
	}
	// truncation counts no width for colors and still neutralizes escapes in text
	tr := truncateWidth(c, 9)
	if stripANSI(tr) != "a.log:3  " {
		t.Errorf("truncate: %q", tr)
	}
	if got := truncateWidth("bad \x1b[31m text", 20); strings.Contains(got, "\x1b") {
		t.Errorf("escape in text passed: %q", got)
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestSpilledRowsKeepPrefix(t *testing.T) {
	l := Line{Text: "a.log:3:x", Src: "a.log", No: 3, Pre: 8, PreName: 6}
	got, err := decodeLine(encodeLine(nil, l), nil)
	if err != nil || got.Pre != 8 || got.PreName != 6 || got.Text != l.Text {
		t.Errorf("round trip: %+v %v", got, err)
	}
}

func TestColorModes(t *testing.T) {
	defer func(m colorSetting) { colorMode = m }(colorMode)
	for _, c := range []struct{ in string }{{"auto"}, {"always"}, {"never"}} {
		if _, err := parseColorMode(c.in); err != nil {
			t.Error(err)
		}
	}
	if _, err := parseColorMode("sometimes"); err == nil {
		t.Error("sometimes: expected an error")
	}
	lines := []Line{{Text: "app.log:3:hello", Src: "app.log", Pre: 10, PreName: 8}}
	print := func(mode colorSetting, active bool) string {
		colorMode = mode
		var buf strings.Builder
		u := &UI{out: bufio.NewWriter(&buf), active: active}
		u.Print(lines)
		u.out.Flush()
		return buf.String()
	}
	if got := print(colorAuto, false); got != "app.log:3:hello\n" {
		t.Errorf("auto, pipe: %q", got)
	}
	if got := print(colorAlways, false); !strings.Contains(got, "\x1b[35mapp.log\x1b[39m") || !strings.Contains(got, "  hello") {
		t.Errorf("always, pipe: %q", got)
	}
	if got := print(colorNever, true); strings.Contains(got, "\x1b") || !strings.HasPrefix(got, "app.log:3  hello") {
		t.Errorf("never, terminal: %q", got)
	}
	if got := truncateWidth(displayLine(lines[0], false, 9), 40); strings.Contains(got, "\x1b") {
		t.Errorf("never, live screen: %q", got)
	}
}

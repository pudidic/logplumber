package main

import "testing"

func TestAnalyze(t *testing.T) {
	sh := &Shell{ui: &UI{}, threads: 1, lang: langShell}
	cases := []struct {
		line    string
		runCmds int // commands in the runnable prefix
		badFrom int
	}{
		{"", 1, -1}, // empty line: the latest events (an SPL search)
		{"  ", 1, -1},
		{"gr", 0, 0},
		{"grep", 0, 0},
		{"grep foo", 1, -1},
		{"grep foo | so", 1, 11},
		{"grep foo | sort", 2, -1},
		{"grep foo | sort |", 2, 16},
		{"grep foo | sort | head -n", 2, 18},
		{"grep foo | sort | head -n 3", 3, -1},
		{"grep '[a' | sort", 0, 0},
		{"grep foo | sed 's/a/b", 1, 11},
		{"grep foo | grep 'abc", 1, 11},
		{"grep foo || wc", 1, 10},
		{"cd ..", 0, -1},
		{"grep a > x", 0, -1},
	}
	for _, c := range cases {
		a := sh.analyze(c.line)
		n := 0
		if a.ast != nil {
			n = len(a.ast.cmds)
		}
		if n != c.runCmds || a.badFrom != c.badFrom {
			t.Errorf("%q: got run=%d bad=%d (%s), want run=%d bad=%d", c.line, n, a.badFrom, a.note, c.runCmds, c.badFrom)
		}
	}
}

func TestParseKeys(t *testing.T) {
	ev, rest := parseKeys([]byte("a\x1b[A\x1b[3~\xed\x95\x9c\r\x1b"))
	kinds := []keyKind{kRune, kUp, kDelete, kRune, kEnter, kEsc}
	if len(ev) != len(kinds) || len(rest) != 0 {
		t.Fatalf("got %v rest %v", ev, rest)
	}
	for i, k := range kinds {
		if ev[i].kind != k {
			t.Errorf("event %d: got %v want %v", i, ev[i].kind, k)
		}
	}
	if ev[3].r != 0xD55C { // U+D55C HANGUL SYLLABLE HAN, encoded as ED 95 9C
		t.Errorf("got rune %q", ev[3].r)
	}
	_, rest = parseKeys([]byte{0xed, 0x95})
	if len(rest) != 2 {
		t.Errorf("incomplete utf-8 not kept: %v", rest)
	}
}

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// capDisplay collects printed lines.
type capDisplay struct {
	mu    sync.Mutex
	lines []string
}

func (d *capDisplay) Begin()                {}
func (d *capDisplay) SetStatus(string)      {}
func (d *capDisplay) SetLive([]string)      {}
func (d *capDisplay) Errorf(string, ...any) {}
func (d *capDisplay) End(string)            {}
func (d *capDisplay) Live() bool            { return false }
func (d *capDisplay) PreviewRows() int      { return 10 }
func (d *capDisplay) Print(ls []Line) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range ls {
		d.lines = append(d.lines, l.Text)
	}
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(text)
	f.Close()
}

// follow runs query in dir, calls step while it follows, then stops it (Ctrl+C)
// and returns the output.
func follow(t *testing.T, dir string, lang langMode, query string, step func()) []string {
	t.Helper()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)
	sh := &Shell{ui: &UI{}, threads: 2, lang: lang}
	var ast *pipelineAST
	var err error
	if lang == langSPL {
		ast, err = splParse(query)
	} else {
		var toks []token
		if toks, err = lex(query); err == nil {
			ast, err = parsePipeline(toks)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	d := &capDisplay{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- sh.runPipeline(ctx, ast, d, false) }()
	time.Sleep(400 * time.Millisecond) // started and at the end of the files
	step()
	time.Sleep(3 * followPoll)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return d.lines
}

func TestFollowSPL(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	appendFile(t, a, "time=2020-01-01T00:00:00Z level=error n=old\n")
	now := time.Now().UTC().Format(time.RFC3339)
	out := follow(t, dir, langSPL, "earliest=rt level=error", func() {
		appendFile(t, a, "time="+now+" level=error n=1\ntime="+now+" level=info n=2\n")
		appendFile(t, a, "time="+now+" level=error n=3 partial") // completed later
		time.Sleep(2 * followPoll)
		appendFile(t, a, "\n")
		appendFile(t, filepath.Join(dir, "b.log"), "time="+now+" level=error n=b\n") // a new file
		time.Sleep(followRescan + 2*followPoll)
	})
	got := strings.Join(out, "|")
	for _, want := range []string{"n=1", "n=3 partial", "n=b"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %q", want, got)
		}
	}
	if strings.Contains(got, "n=old") || strings.Contains(got, "n=2") {
		t.Errorf("old or filtered lines in %q", got)
	}
	if !strings.HasPrefix(got, "time="+now+" level=error n=1|") {
		t.Errorf("events should come in arrival order: %q", got)
	}
}

func TestFollowStatsAndWindow(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339)
	appendFile(t, a, "time="+old+" level=x\ntime="+recent+" level=x\n")
	now := time.Now().UTC().Format(time.RFC3339)
	out := follow(t, dir, langSPL, "earliest=rt-1m latest=rt | stats count by level", func() {
		appendFile(t, a, "time="+now+" level=x\ntime="+now+" level=y\n")
	})
	// the line from an hour ago is outside the window; the recent one is backfilled
	if got := strings.Join(strings.Fields(strings.Join(out, " ")), " "); got != "x 2 y 1" { // rows only: capDisplay does not print headers
		t.Errorf("window stats: %q", got)
	}
}

func TestFollowTail(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	appendFile(t, a, "one\ntwo\nthree\n")
	out := follow(t, dir, langShell, "tail -f -n 2 a.log b.log", func() {
		appendFile(t, a, "four\n")
		appendFile(t, filepath.Join(dir, "b.log"), "bee\n") // did not exist at the start
		time.Sleep(followRescan + 2*followPoll)
		os.WriteFile(a, []byte("again\n"), 0o644) // truncated: read from the start
		time.Sleep(2 * followPoll)
	})
	if got := strings.Join(out, " "); got != "two three four bee again" {
		t.Errorf("tail -f: %q", got)
	}
}

func TestWindowStage(t *testing.T) {
	s := &windowStage{window: time.Minute}
	now := time.Now()
	for _, d := range []time.Duration{-2 * time.Minute, -30 * time.Second, -5 * time.Second} {
		s.Push(Line{Text: now.Add(d).UTC().Format(time.RFC3339Nano) + " x"}, nil)
	}
	s.Push(Line{Text: "no time: arrival time counts"}, nil)
	var got []string
	s.Snapshot(func(l Line) bool { got = append(got, l.Text); return true })
	if len(got) != 3 || strings.Contains(got[0], now.Add(-2*time.Minute).UTC().Format("15:04:05")) {
		t.Errorf("window: %q", got)
	}
}

func TestRealtimeSyntax(t *testing.T) {
	ok := []string{"earliest=rt", "earliest=rt latest=rt", "earliest=rt-5m latest=rt error", "earliest=rt-1h@h"}
	for _, q := range ok {
		_, p, err := parseSearch(q, 0, true)
		if err != nil || p.rt == nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	if _, p, _ := parseSearch("earliest=rt-5m", 0, true); p.rt.window != 5*time.Minute || !p.rt.backfill {
		t.Errorf("window: %+v", p.rt)
	}
	for _, q := range []string{"latest=rt", "earliest=rt latest=now", "earliest=rt+5m", "earliest=rtx", "(earliest=rt)", "NOT earliest=rt"} {
		if _, _, err := parseSearch(q, 0, true); err == nil {
			t.Errorf("%s: expected an error", q)
		}
	}
	if _, _, err := parseSearch("earliest=rt", 0, false); err == nil {
		t.Error("rt after the first search: expected an error")
	}
	for _, q := range []string{"grep x | tail -f", "tail -f -n +2 a"} {
		toks, _ := lex(q)
		ast, _ := parsePipeline(toks)
		if _, err := (&Shell{lang: langShell}).plan(ast, nil); err == nil {
			t.Errorf("%s: expected an error", q)
		}
	}
}

func (d *capDisplay) SetTimeline(*timeline) {}
func (d *capDisplay) SetColumns(bool)       {}

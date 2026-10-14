package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// sumDisplay collects result lines and the summary line.
type sumDisplay struct {
	captureDisplay
	mu      sync.Mutex
	summary string
}

func (d *sumDisplay) End(s string) { d.mu.Lock(); d.summary = s; d.mu.Unlock() }

// indexFixtures writes logs large enough for many index chunks: plain files,
// a gzip file and archive members, with rare and common words.
func indexFixtures(t *testing.T, dir string) {
	t.Helper()
	rnd := rand.New(rand.NewSource(7))
	words := []string{"alpha", "beta", "gamma", "delta", "ok", "done", "cache", "hit"}
	users := []string{"kim", "lee", "park", "KIM"}
	gen := func(n int, prefix string) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			ts := fmt.Sprintf("2026-10-14T09:%02d:%02dZ", i/60%60, i%60)
			switch r := rnd.Intn(100); {
			case r < 2:
				fmt.Fprintf(&b, "%s level=error user=%s msg=\"db timeout\" status=503 %s\n", ts, users[rnd.Intn(4)], prefix)
			case r < 4:
				fmt.Fprintf(&b, "%s level=warn user=%s msg=\"connection refused\" status=502\n", ts, users[rnd.Intn(4)])
			case r < 5:
				fmt.Fprintf(&b, "%s needle %d found ÉRROR Fail-over\n", ts, i)
			case r < 8:
				fmt.Fprintf(&b, `{"ts":"%s","level":"error","user":"%s","msg":"failure %d"}`+"\n", ts, users[rnd.Intn(4)], i)
			default:
				fmt.Fprintf(&b, "%s level=info user=%s msg=\"%s %s\" status=200\n", ts, users[rnd.Intn(4)], words[rnd.Intn(8)], words[rnd.Intn(8)])
			}
		}
		return b.String()
	}
	write := func(name string, data []byte) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/app.log", []byte(gen(3000, "a")))
	write("b/other.log", []byte(gen(1500, "b")))
	write("c/old.log.gz", gzipBytes([]byte(gen(1500, "c"))))
	writeFixtures(t, dir) // tar.gz, zip and binary members
}

func runQuery(t *testing.T, sh *Shell, q string) ([]string, string) {
	t.Helper()
	var ast *pipelineAST
	var err error
	if sh.lang == langSPL {
		ast, err = splParse(q)
	} else {
		var toks []token
		if toks, err = lex(q); err == nil {
			ast, err = parsePipeline(toks)
		}
	}
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	d := &sumDisplay{}
	if err := sh.runPipeline(context.Background(), ast, d, false); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	sort.Strings(d.lines) // files are read in parallel; equal times have no fixed order
	return d.lines, d.summary
}

func TestIndexedQueriesMatchFullReads(t *testing.T) {
	old := chunkSize
	chunkSize = 4 << 10 // many chunks per file
	defer func() { chunkSize = old }()
	dir := t.TempDir()
	indexFixtures(t, dir)
	t.Chdir(dir)
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	st.setRoot(".")
	waitIndexed(t, st)

	cases := []struct {
		lang      langMode
		q         string
		narrowing bool // the index must skip some text
	}{
		{langSPL, `needle`, true},
		{langSPL, `"db timeout"`, true},
		{langSPL, `"DB TIMEOUT"`, true},
		{langSPL, `user=kim level=error`, true},
		{langSPL, `user=KIM`, true},
		{langSPL, `refused OR needle`, true},
		{langSPL, `need*`, true},
		{langSPL, `*eedl*`, true},
		{langSPL, `(level=error user=park) OR "connection refused"`, true},
		{langSPL, `needle | stats count by _src`, true},
		{langSPL, `needle | head 3`, true},
		{langSPL, `failure | table _line _src msg`, true},
		{langSPL, `source=*.gz needle`, true},
		{langSPL, `level=error`, false}, // common: may not skip anything
		{langSPL, `ÉRROR`, false},       // non-ASCII: no index
		{langSPL, `NOT needle | stats count`, false},
		{langSPL, `status>=503 | stats count`, false},
		{langSPL, `ok`, false}, // shorter than a trigram
		{langSPL, `zzzzqqq`, true},
		{langSPL, `ERROR`, false},
		{langShell, `grep needle`, true},
		{langShell, `grep -n -i NEEDLE`, true},
		{langShell, `grep -l timeout`, true},
		{langShell, `grep -c needle`, false}, // counts per file, also zero: reads all
		{langShell, `grep -v needle | wc -l`, false},
		{langShell, `grep -F 'db timeout' | sort | uniq -c`, true},
		{langShell, `grep 'need.e'`, false}, // a regex: no index
	}
	full := &Shell{ui: &UI{}, threads: 2}
	indexed := &Shell{ui: &UI{}, threads: 2, store: st}
	for _, c := range cases {
		full.lang, indexed.lang = c.lang, c.lang
		want, _ := runQuery(t, full, c.q)
		got, summary := runQuery(t, indexed, c.q)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: indexed results differ: %d lines, want %d\n got  %.300q\n want %.300q", c.q, len(got), len(want), got, want)
		}
		if c.narrowing && !strings.Contains(summary, "index: read") {
			t.Errorf("%s: index not used (%s)", c.q, summary)
		}
		if c.narrowing && strings.Contains(summary, "index: read") {
			var r, total string
			fmt.Sscanf(summary[strings.Index(summary, "index: read")+len("index: read "):], "%s of %s", &r, &total)
			if r == total {
				t.Errorf("%s: the index skipped nothing (%s)", c.q, summary)
			}
		}
	}
}

func TestIndexNeeds(t *testing.T) {
	cases := []struct{ q, want string }{
		{`error`, "[[error]]"},
		{`"DB timeout"`, "[[db timeout]]"},
		{`error user=kim`, "[[error kim]]"},
		{`a OR b`, "none"},
		{`error OR refused`, "[[error] [refused]]"},
		{`(x1y OR abc) (def OR ghi)`, "[[x1y def] [x1y ghi] [abc def] [abc ghi]]"},
		{`NOT error`, "none"},
		{`error NOT abc`, "[[error]]"},
		{`fail*ure`, "[[fail ure]]"},
		{`status>=500`, "none"},
		{`status=500`, "none"}, // numbers are compared numerically (500 = 500.0)
		{`level IN (warn, error)`, "none"},
		{`earliest=-1h error`, "[[error]]"},
		{`*`, "none"},
		{`ÉRROR`, "none"},
	}
	for _, c := range cases {
		n, _, err := parseSearchText(c.q, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		alts, ok := indexNeeds(n)
		got := "none"
		if ok {
			got = fmt.Sprint(alts)
		}
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.q, got, c.want)
		}
	}
}

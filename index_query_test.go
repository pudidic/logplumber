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
				fmt.Fprintf(&b, `{"ts":"%s","level":"error","user":"%s","msg":"failure %d","trace":"%016x"}`+"\n", ts, users[rnd.Intn(4)], i, rnd.Uint64())
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
	app := gen(3000, "a")
	i := strings.Index(app, `"trace":"`) + 9
	traceID = app[i : i+16]
	write("a/app.log", []byte(app))
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

// traceID is one of the trace IDs in indexFixtures (set when they are written).
var traceID string

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
		{langSPL, traceID, true},            // hex: trigrams everywhere, the word filter narrows it
		{langSPL, `trace=` + traceID, true}, // as field=value
		{langSPL, `xq`, true},               // a short word: word filters only
		{langSPL, `ok`, false},
		{langSPL, `kim | stats count`, false},
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
	_ = traceID
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
		{`error`, "[error|error]"},
		{`"DB timeout"`, "[db timeout|db timeout]"},
		{`error user=kim`, "[error kim|error kim]"},
		{`a OR b`, "[|a] [|b]"}, // short words: words only
		{`error OR refused`, "[error|error] [refused|refused]"},
		{`(x1y OR abc) (def OR ghi)`, "[x1y def|x1y def] [x1y ghi|x1y ghi] [abc def|abc def] [abc ghi|abc ghi]"},
		{`NOT error`, "none"},
		{`error NOT abc`, "[error|error]"},
		{`fail*ure`, "[fail ure|]"}, // wildcard pieces are parts of words
		{`fa*`, "none"},
		{`INC-77fe31`, "[inc-77fe31|inc 77fe31]"},
		{`trace=7f3a9c0e`, "[7f3a9c0e|7f3a9c0e]"},
		{`status>=500`, "none"},
		{`status=500`, "none"}, // numbers are compared numerically (500 = 500.0)
		{`level IN (warn, error)`, "none"},
		{`earliest=-1h error`, "[error|error]"},
		{`ok`, "[|ok]"},
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
			var parts []string
			for _, a := range alts {
				parts = append(parts, "["+strings.Join(a.lits, " ")+"|"+strings.Join(a.words, " ")+"]")
			}
			got = strings.Join(parts, " ")
		}
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.q, got, c.want)
		}
	}
}

func TestWordFilter(t *testing.T) {
	words := map[uint64]struct{}{}
	for _, w := range []string{"error", "kim", "7f3a9c0e5b", "ok"} {
		words[wordHash(w)] = struct{}{}
	}
	f := buildBloom(words)
	for _, w := range []string{"error", "ERROR", "kim", "7f3a9c0e5b", "ok"} {
		if !bloomHas(f, []uint64{wordHash(w)}) {
			t.Errorf("%s: missing", w)
		}
	}
	// false positives are possible but rare at 10 bits per word
	fp := 0
	for i := 0; i < 1000; i++ {
		if bloomHas(f, []uint64{wordHash(fmt.Sprintf("absent%d", i))}) {
			fp++
		}
	}
	if fp > 50 {
		t.Errorf("%d false positives in 1000", fp)
	}
	if got := strings.Join(asciiWords("INC-77fe31 user=kim, ÉRROR_x"), " "); got != "INC 77fe31 user kim RROR_x" {
		t.Errorf("asciiWords: %q", got)
	}
}

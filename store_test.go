package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureDisplay collects a pipeline's result lines.
type captureDisplay struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureDisplay) Begin()                {}
func (c *captureDisplay) SetStatus(string)      {}
func (c *captureDisplay) SetLive([]string)      {}
func (c *captureDisplay) Errorf(string, ...any) {}
func (c *captureDisplay) End(string)            {}
func (c *captureDisplay) Live() bool            { return false }
func (c *captureDisplay) PreviewRows() int      { return 10 }
func (c *captureDisplay) Print(l []string) {
	c.mu.Lock()
	c.lines = append(c.lines, l...)
	c.mu.Unlock()
}

func runLine(t *testing.T, sh *Shell, line string) []string {
	t.Helper()
	toks, err := lex(line)
	if err != nil {
		t.Fatal(err)
	}
	ast, err := parsePipeline(toks)
	if err != nil {
		t.Fatal(err)
	}
	d := &captureDisplay{}
	if err := sh.runPipeline(context.Background(), ast, d, false); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	sort.Strings(d.lines) // files are read in parallel
	return d.lines
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// writeFixtures creates plain, gzip, tar.gz and zip files with known lines.
func writeFixtures(t *testing.T, dir string) {
	t.Helper()
	write := func(name string, data []byte) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("plain.log", []byte("alpha one\nbeta two\nERROR plain\n"))
	write("sub/single.log.gz", gzipBytes([]byte("gz line\nERROR in gz\n")))

	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	for i, body := range []string{"tar first\nERROR tar a\n", "tar second\n"} {
		tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("in/m%d.txt", i), Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	write("arc/bundle.tar.gz", gzipBytes(tb.Bytes()))

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	f, _ := zw.Create("z/inside.txt")
	f.Write([]byte("zip line\nERROR zip\n"))
	zw.Close()
	write("arc/z.zip", zb.Bytes())
	write("bin.dat", []byte("bin\x00ary"))
}

func waitIndexed(t *testing.T, s *store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.status.Load(); st != nil && st.root != "" && !st.building && st.files > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("index not ready")
}

func TestStoreCacheMatchesDirectReads(t *testing.T) {
	dir := t.TempDir()
	writeFixtures(t, dir)
	t.Chdir(dir)

	plain := &Shell{ui: &UI{}, threads: 2}
	queries := []string{"grep ERROR", "grep -c line", "wc -l", "find -name '*.txt'", "cat *.txt"}
	want := map[string][]string{}
	for _, q := range queries {
		want[q] = runLine(t, plain, q)
	}

	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	st.setRoot(".")
	waitIndexed(t, st)
	indexed := &Shell{ui: &UI{}, threads: 2, store: st}

	snap := st.snap.Load()
	cached := 0
	for _, e := range snap.srcs {
		for _, u := range e.units {
			if strings.HasPrefix(u.path, st.dir) {
				cached++
			}
		}
	}
	if cached != 4 { // single.log.gz, two tar members, one zip member
		t.Errorf("cached units = %d, want 4", cached)
	}
	for _, q := range queries {
		if got := runLine(t, indexed, q); strings.Join(got, "\n") != strings.Join(want[q], "\n") {
			t.Errorf("%s:\n got  %q\n want %q", q, got, want[q])
		}
	}

	storeDir := st.dir
	if err := st.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(storeDir); !os.IsNotExist(err) {
		t.Errorf("store directory not removed: %v", err)
	}
}

func TestTrigramCandidates(t *testing.T) {
	old := chunkSize
	chunkSize = 16 // several chunks per file
	defer func() { chunkSize = old }()
	dir := t.TempDir()
	writeFixtures(t, dir)
	os.WriteFile(filepath.Join(dir, "long.log"), []byte(strings.Repeat("filler line\n", 20)+"needle here\n"+strings.Repeat("filler line\n", 20)), 0o644)
	t.Chdir(dir)
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	st.setRoot(".")
	waitIndexed(t, st)
	snap := st.snap.Load()

	units := map[int32]string{}
	for p, e := range snap.srcs {
		for _, u := range e.units {
			units[u.id] = filepath.Base(p) + u.member
		}
	}
	names := func(term string) []string {
		cand, ok := snap.candidates([][]byte{[]byte(term)})
		if !ok {
			t.Fatalf("%s: index not usable", term)
		}
		var out []string
		for id := range cand {
			out = append(out, units[id])
		}
		sort.Strings(out)
		return out
	}
	want := "bundle.tar.gz::in/m0.txt plain.log single.log.gz z.zip::z/inside.txt"
	if got := strings.Join(names("error"), " "); got != want {
		t.Errorf("error: got %q want %q", got, want)
	}
	// Only the chunk holding the needle is a candidate, with its line number.
	cand, _ := snap.candidates([][]byte{[]byte("needle")})
	var chunks []chunkRec
	for id, cs := range cand {
		if units[id] == "long.log" {
			chunks = cs
		}
	}
	if len(chunks) != 1 || chunks[0].line != 20 && chunks[0].line != 21 {
		t.Errorf("needle chunks: %+v", chunks)
	}
	if _, ok := snap.candidates([][]byte{[]byte("ab")}); ok {
		t.Error("a 2-byte term must not use the index")
	}
}

func TestStaleStoreCleanup(t *testing.T) {
	stale, err := os.MkdirTemp("", storePrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(stale, "lock"), nil, 0o644) // lock file nobody holds
	cleanStaleStores()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale store not removed")
		os.RemoveAll(stale)
	}
}

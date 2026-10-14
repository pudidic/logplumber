package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// persistQuery runs an SPL query with and without the store and checks that
// both agree; it returns the result and whether the index was used.
func persistQuery(t *testing.T, st *store, q string) ([]string, bool) {
	t.Helper()
	want, _ := runQuery(t, &Shell{ui: &UI{}, threads: 2}, q)
	got, sum := runQuery(t, &Shell{ui: &UI{}, threads: 2, store: st}, q)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s: indexed %d lines, full read %d", q, len(got), len(want))
	}
	return got, strings.Contains(sum, "index: read")
}

func TestPersistentIndex(t *testing.T) {
	data, idx := t.TempDir(), t.TempDir()
	indexFixtures(t, data)
	t.Chdir(data)

	// 1st session: index everything
	st, err := openPersistentStore(idx)
	if err != nil {
		t.Fatal(err)
	}
	st.setRoot(".")
	waitIndexed(t, st)
	files := st.status.Load().files
	r1, used := persistQuery(t, st, traceID)
	if !used || len(r1) != 1 {
		t.Fatalf("session 1: %d lines, index used %v", len(r1), used)
	}
	if err := st.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(idx, manifestName)); err != nil {
		t.Fatal("no manifest after close")
	}

	// 2nd session: usable at once, nothing to index again
	st, err = openPersistentStore(idx)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.status.Load().files; got != files {
		t.Errorf("restored %d files, want %d", got, files)
	}
	r2, used := persistQuery(t, st, traceID) // before any pass
	if !used || strings.Join(r2, "") != strings.Join(r1, "") {
		t.Errorf("session 2 before a pass: %q, index used %v", r2, used)
	}
	st.setRoot(".")
	waitIndexed(t, st)
	if s := st.status.Load(); s.total != 0 {
		t.Errorf("session 2 re-indexed %d bytes", s.total)
	}

	// a 2nd psh on the same index gets a temporary one
	t.Setenv("PSH_INDEX_DIR", idx)
	other, note, err := openIndex()
	if err != nil || other.persistent || !strings.Contains(note, "temporary") {
		t.Errorf("second psh: persistent=%v note=%q err=%v", other != nil && other.persistent, note, err)
	}
	if other != nil {
		other.close()
	}
	st.close()

	// change one file, delete another: the 3rd session indexes only the change
	appendFile(t, filepath.Join(data, "b/other.log"), "2026-10-14T10:00:00Z level=error msg=\"brand new zebra-42\"\n")
	os.Remove(filepath.Join(data, "plain.log"))
	st, err = openPersistentStore(idx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	st.setRoot(".")
	waitIndexed(t, st)
	if s := st.status.Load(); s.total == 0 || s.total > 1<<20 {
		t.Errorf("session 3 indexed %d bytes, want only the changed file", s.total)
	}
	if r, used := persistQuery(t, st, "zebra"); len(r) != 1 || !used {
		t.Errorf("new line: %q, index used %v", r, used)
	}
	persistQuery(t, st, "ERROR") // plain.log's line is gone in both
	persistQuery(t, st, traceID)
}

func TestPersistentIndexRecovers(t *testing.T) {
	data, idx := t.TempDir(), t.TempDir()
	indexFixtures(t, data)
	t.Chdir(data)
	st, _ := openPersistentStore(idx)
	st.setRoot(".")
	waitIndexed(t, st)
	st.close()

	// a damaged manifest: start over
	os.WriteFile(filepath.Join(idx, manifestName), []byte("garbage"), 0o644)
	st, err := openPersistentStore(idx)
	if err != nil {
		t.Fatal(err)
	}
	if n := st.status.Load().files; n != 0 {
		t.Errorf("a damaged index kept %d files", n)
	}
	st.setRoot(".")
	waitIndexed(t, st)
	persistQuery(t, st, traceID)
	st.close()

	// truncated data files: start over
	os.Truncate(filepath.Join(idx, bloomFile), 10)
	st, _ = openPersistentStore(idx)
	if n := st.status.Load().files; n != 0 {
		t.Errorf("truncated word filters kept %d files", n)
	}
	st.close()

	// mostly stale (every file changed): start over
	old := staleMinBytes
	staleMinBytes = 0
	defer func() { staleMinBytes = old }()
	st, _ = openPersistentStore(idx)
	st.setRoot(".")
	waitIndexed(t, st)
	st.close()
	filepath.Walk(data, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			appendFile(t, p, "x\n")
		}
		return nil
	})
	st, _ = openPersistentStore(idx)
	defer st.close()
	if n := st.status.Load().files; n != 0 {
		t.Errorf("a stale index kept %d files", n)
	}

	// index clear
	st.setRoot(".")
	waitIndexed(t, st)
	if err := st.clear(data); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, st)
	persistQuery(t, st, "needle")
}

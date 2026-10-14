package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var searchLogs = []string{
	`2026-10-14T09:00:01Z level=error msg="db timeout" status=503 user=kim`,
	`2026-10-14T09:00:02Z level=info msg="cache hit" status=200 user=lee`,
	`2026-10-14T09:00:03Z level=warn msg="slow query" status=200 user=Kim`,
	`2026-10-14T09:00:04Z level=error msg="payment declined" status=502 user=park`,
	`no fields here, just errors`,
}

func TestSearchLanguage(t *testing.T) {
	cases := []struct {
		q    string
		want string // the users of the matching lines, "-" for the line without fields
	}{
		{`error`, "kim park"}, // whole words: "errors" does not match
		{`error*`, "kim park -"},
		{`ERROR`, "kim park"},
		{`"db timeout"`, "kim"},
		{`level=error status>=503`, "kim"},
		{`status>500`, "kim park"},
		{`status!=200`, "kim park"},
		{`NOT status=200`, "kim park -"},
		{`user=kim`, "kim Kim"}, // values are case-insensitive
		{`user=k*`, "kim Kim"},
		{`user=*`, "kim lee Kim park"},
		{`level IN (warn, info)`, "lee Kim"},
		{`level=warn OR level=info status=200`, "lee Kim"},
		{`level=error OR level=info user=lee`, "lee"}, // OR binds tighter than AND
		{`(level=error user=park) OR user=lee`, "lee park"},
		{`level=error AND NOT "db timeout"`, "park"},
		{`*`, "kim lee Kim park -"},
		{`index=main error`, "kim park"},
		{`earliest=2026-10-14T09:00:02 latest=2026-10-14T09:00:04`, "lee Kim"},
		{`msg="cache*"`, "lee"},
	}
	for _, c := range cases {
		n, _, err := parseSearchText(c.q, 0, true)
		if err != nil {
			t.Errorf("%s: %v", c.q, err)
			continue
		}
		var got []string
		for _, in := range searchLogs {
			l := Line{Text: in}
			if n.match(&searchCtx{l: &l}) {
				u, ok := l.Field("user")
				if !ok {
					u = "-"
				}
				got = append(got, u)
			}
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s: got %q, want %q", c.q, strings.Join(got, " "), c.want)
		}
	}
	for _, bad := range []string{`status>`, `(a`, `a)`, `level IN (a`, `AND a`, `earliest=-3x`, `"open`} {
		if _, _, err := parseSearchText(bad, 0, true); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
}

func TestSearchSources(t *testing.T) {
	_, src, err := parseSearchText(`source=*.jsonl error source="app/a.log"`, 0, true)
	if err != nil || strings.Join(src, " ") != "*.jsonl app/a.log" {
		t.Errorf("sources: %q %v", src, err)
	}
	// inside OR, or after the first command, source= filters events by _src
	n, src, _ := parseSearchText(`source=a.log OR source=b.log`, 0, true)
	l := Line{Text: "x", Src: "dir/b.log"}
	if len(src) != 0 || n.match(&searchCtx{l: &l}) {
		t.Errorf("source in OR: %q", src)
	}
	n, src, _ = parseSearchText(`source=*b.log`, 0, false)
	if len(src) != 0 || !n.match(&searchCtx{l: &l}) {
		t.Errorf("source after the first command: %q", src)
	}
}

func TestSplitSPL(t *testing.T) {
	cases := []struct{ q, want string }{
		{`error | stats count`, "search[error] stats[count]"},
		{`search error|head 5`, "search[error] head[5]"},
		{`"a|b" | where x = "c|d"`, `search["a|b"] where[x = "c|d"]`},
		{`it's broken | eval y = 'x y'`, `search[it's broken] eval[y = 'x y']`},
		{`  reverse  `, "search[reverse]"},
		{`x | reverse`, "search[x] reverse[]"},
	}
	for _, c := range cases {
		var got []string
		for _, s := range splitSPL(c.q) {
			if s.err != nil {
				t.Errorf("%s: %v", c.q, s.err)
			}
			raw, _ := rawArg(s.cmd.args)
			got = append(got, s.cmd.args[0].s+"["+raw+"]")
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s: got %q, want %q", c.q, strings.Join(got, " "), c.want)
		}
	}
}

func TestImplicitTimeOrder(t *testing.T) {
	names := func(q string, live bool) string {
		ast, err := splParse(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var specs []*Spec
		for i, c := range ast.cmds {
			s, _, err := buildSPLSpec(c, i == 0)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			specs = append(specs, s)
		}
		var out []string
		ordered, _ := orderSPL(specs, live, nil)
		for _, s := range ordered {
			n := s.Name
			if f, ok := s.New().(*fieldSortStage); ok && f.limit > 0 {
				n += "*" // a bounded sort
			}
			out = append(out, n)
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		q    string
		live bool
		want string
	}{
		{`error`, false, "search timeline sort"},
		{`error`, true, "search timeline sort*"},
		{`error | where x > 1 | head 5`, false, "search where timeline sort* head"},
		{`error | dedup user | table user`, false, "search timeline sort dedup table"},
		{`error | stats count`, false, "search stats"},
		{`error | sort user`, false, "search sort"},
		{`error | stats count by x | rename count AS n`, false, "search stats rename retable"},
	}
	for _, c := range cases {
		if got := names(c.q, c.live); got != c.want {
			t.Errorf("%s (live=%v): got %q, want %q", c.q, c.live, got, c.want)
		}
	}
}

func TestSPLCommands(t *testing.T) {
	cases := []struct{ pipe, want string }{
		{`dedup level | table user`, "user kim lee Kim"},
		{`dedup 2 level | table user`, "user kim lee Kim park"},
		{`dedup level consecutive=true | table level`, "level error info warn error"},
		{`regex user="^[A-Z]" | table user`, "user Kim"},
		{`regex user!="^[A-Z]" | table user`, "user kim lee park"},
		{`regex "db|payment" | table user`, "user kim park"},
		{`stats count by level | rename count AS n | sort -n`, "level n error 2 info 1 warn 1"},
		{`stats count by level | eval twice = count * 2 | fields level, twice`, "level twice error 4 info 2 warn 2"},
		{`stats count by level | fields - count`, "level error info warn"},
		{`fields user | table *`, "_time user 2026-10-14 09:00:01 kim 2026-10-14 09:00:02 lee 2026-10-14 09:00:03 Kim 2026-10-14 09:00:04 park"},
		{`fields - msg, status, level | head 1 | table *`, "_time sourcetype user 2026-10-14 09:00:01 kv kim"},
		{`rename user AS who | head 1 | table who user`, "who user kim"},
		{`table user, st*`, "user status kim 503 lee 200 Kim 200 park 502"},
		{`sort - status, user | head 2 | table user`, "user kim park"},
		{`sort num(status) | head 1 | table user`, "user lee"},
		{`reverse | head 1 | table user`, "user"},
		{`search status=200 | head 1 | table user`, "user lee"},
		{`tail 1 | table _raw`, "_raw no fields here, just errors"},
	}
	utc(t)
	for _, c := range cases {
		got := strings.Join(strings.Fields(strings.Join(runText(t, c.pipe, searchLogs...), " ")), " ")
		if got != c.want {
			t.Errorf("%s\n got  %q\n want %q", c.pipe, got, c.want)
		}
	}
}

func TestOutputCSV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out")
	runText(t, `stats count by level | outputcsv `+path, searchLogs...)
	b, err := os.ReadFile(path + ".csv")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "level,count\nerror,2\ninfo,1\nwarn,1\n" {
		t.Errorf("csv: %q", got)
	}
	runText(t, `search user=lee | outputcsv `+path+".csv", searchLogs...)
	b, _ = os.ReadFile(path + ".csv")
	if !strings.HasPrefix(string(b), "_time,_src,_raw\n1791968402,test.log,") {
		t.Errorf("event csv: %q", b)
	}
}

func TestShellMode(t *testing.T) {
	out := runShell(t, `grep -v here | cut -d' ' -f2 | sort | uniq -c | sort -rn`, searchLogs...)
	if strings.Join(strings.Fields(strings.Join(out, " ")), " ") != "2 level=error 1 level=warn 1 level=info" {
		t.Errorf("shell pipeline: %q", out)
	}
	for _, c := range []string{"where x > 1", "grep x | stats count", "grep x | timechart count"} {
		toks, _ := lex(c)
		ast, err := parsePipeline(toks)
		if err != nil {
			continue // "where x > 1" is a redirection in shell syntax
		}
		_, err = (&Shell{lang: langShell}).plan(ast, nil)
		if err == nil || !strings.Contains(err.Error(), "SPL command") {
			t.Errorf("%s: want an SPL-command error, got %v", c, err)
		}
	}
	if _, _, err := buildSPLSpec(rawCmd{args: []word{{s: "grep"}}}, false); err == nil || !strings.Contains(err.Error(), "shell command") {
		t.Errorf("grep in SPL: %v", err)
	}
}

func TestDetailFields(t *testing.T) {
	utc(t)
	join := func(ls []string) string {
		var out []string
		for _, s := range ls {
			out = append(out, strings.Join(strings.Fields(strings.TrimPrefix(s, "\x00D")), " "))
		}
		return strings.Join(out, "|")
	}
	l := Line{Text: `2026-10-14T09:00:01Z level=error msg="db timeout"`, Src: "app.log", No: 7}
	want := "_time 2026-10-14 09:00:01 (1791968401)|_src app.log|_line 7|sourcetype kv||level error|msg db timeout"
	if got := join(fieldLines(l, 80)); got != want {
		t.Errorf("event:\n got  %q\n want %q", got, want)
	}
	l = Line{Text: "[Wed Oct 14 09:00:01 2026] [core:error] no fields", Src: "error.log", No: 1}
	if got := join(fieldLines(l, 80)); !strings.HasSuffix(got, "(no other fields: JSON and key=value are extracted automatically; add more with rex)") {
		t.Errorf("no fields: %q", got)
	}
	// table rows keep the column order
	var row Line
	makeTable([]string{"level", "count"}, [][]string{{"error", "2"}}, func(x Line) bool { row = x; return true })
	if got := join(fieldLines(row, 80)); got != "level error|count 2" {
		t.Errorf("row: %q", got)
	}
	// long values wrap under the value column
	l = Line{Text: `k="` + strings.Repeat("x", 30) + `"`}
	if got := fieldLines(l, 30); len(got) != 4 || got[3] != "              "+strings.Repeat("x", 14) {
		t.Errorf("wrap: %q", got)
	}
}

func TestEnterShowsAllEvents(t *testing.T) {
	// The type-ahead preview keeps the newest 1000 events; Enter (the normal
	// terminal display, which also draws live previews) must not.
	limitOf := func(d display) int {
		ast, _ := splParse("error")
		r, err := (&Shell{}).plan(ast, d)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range r.specs {
			if f, ok := s.New().(*fieldSortStage); ok {
				return f.limit
			}
		}
		return -1
	}
	if n := limitOf(&UI{active: true}); n != 0 {
		t.Errorf("Enter: sort limit %d, want 0 (all events)", n)
	}
	if n := limitOf(&liveRun{}); n != liveEventLimit {
		t.Errorf("type-ahead: sort limit %d, want %d", n, liveEventLimit)
	}
}

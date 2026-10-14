package main

import (
	"strings"
	"testing"
)

// runText runs a pipeline over the given input lines and returns the output text.
func runText(t *testing.T, pipeline string, input ...string) []string {
	t.Helper()
	toks, err := lex(pipeline)
	if err != nil {
		t.Fatalf("%s: %v", pipeline, err)
	}
	ast, err := parsePipeline(toks)
	if err != nil {
		t.Fatalf("%s: %v", pipeline, err)
	}
	var specs []*Spec
	for i, c := range ast.cmds {
		s, _, err := buildSpec(c, i == 0)
		if err != nil {
			t.Fatalf("%s: %v", pipeline, err)
		}
		if s.Setup != nil {
			s.Setup(planCtx{}, s)
		}
		specs = append(specs, s)
	}
	st, emits := instantiate(specs)
	var out []string
	var hdr *tableHeader
	chain(st, emits, func(l Line) bool {
		if l.Rec != nil && l.Rec.header != nil && l.Rec.header != hdr {
			hdr = l.Rec.header
			out = append(out, strings.TrimRight(hdr.text, " "))
		}
		out = append(out, strings.TrimRight(l.Text, " "))
		return true
	})
	for i, in := range input {
		emits[0](Line{Text: in, Src: "test.log", No: i + 1})
	}
	for i, s := range st {
		s.Finish(emits[i+1])
	}
	return out
}

func fields(rows []string) [][]string {
	out := make([][]string, len(rows))
	for i, r := range rows {
		out[i] = strings.Fields(r)
	}
	return out
}

var jsonLogs = []string{
	`{"level":"info","req":{"path":"/a"},"status":200,"ms":10,"user":"kim"}`,
	`{"level":"error","req":{"path":"/b"},"status":500,"ms":3000,"user":"lee"}`,
	`{"level":"info","req":{"path":"/b"},"status":200,"ms":30,"user":"kim"}`,
	`2026-10-14 WARN {"level":"warn","req":{"path":"/b"},"status":503,"ms":1500,"user":"park"}`,
	`time=1 level=error status=502 ms=700 user=kim`,
	`plain text without fields`,
}

func TestWhereAndTable(t *testing.T) {
	got := fields(runText(t, `where status >= 500 and ms > 1000 | table user status`, jsonLogs...))
	want := [][]string{{"user", "status"}, {"lee", "500"}, {"park", "503"}}
	if strings.Join(flat(got), " ") != strings.Join(flat(want), " ") {
		t.Errorf("got %v want %v", got, want)
	}
}

func flat(x [][]string) []string {
	var out []string
	for _, r := range x {
		out = append(out, r...)
	}
	return out
}

func TestStats(t *testing.T) {
	got := flat(fields(runText(t, `stats count, avg(ms) as avg, max(ms), p50(ms) by level`, jsonLogs...)))
	want := "level count avg max(ms) p50(ms) error 2 1850 3000 1850 info 2 20 30 20 warn 1 1500 1500 1500"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %q\nwant %q", strings.Join(got, " "), want)
	}
}

func TestStatsPartialMerge(t *testing.T) {
	// Two worker-side instances pre-aggregate; the serial instance merges them.
	c := &statsCfg{aggs: []aggSpec{{fn: "count", name: "count"}, {fn: "dc", field: "user", name: "dc"}}, by: []string{"level"}}
	spec := statsSpec("stats", c)
	var partials []Line
	collect := func(l Line) bool { partials = append(partials, l); return true }
	for _, half := range [][]string{jsonLogs[:3], jsonLogs[3:]} {
		w := spec.NewPartial()
		for _, s := range half {
			w.Push(Line{Text: s}, collect)
		}
		w.Finish(collect)
	}
	m := spec.New()
	var rows []string
	for _, p := range partials {
		m.Push(p, nil)
	}
	m.Finish(func(l Line) bool { rows = append(rows, l.Text); return true })
	got := strings.Join(flat(fields(rows)), " ")
	if want := "error 2 2 info 2 1 warn 1 1"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestEvalTopSortRex(t *testing.T) {
	cases := []struct{ pipe, want string }{
		{`eval slow = if(ms > 1000, "slow", "ok") | stats count by slow`, "slow count ok 4 slow 2"},
		{`top 1 user`, "user count percent kim 3 60.00"},
		{`stats count by user | sort -count user | head 2`, "user count kim 3 lee 1"},
		{`rex "status=(?<code>\d+)" | where isnotnull(code) | table code`, "code 502"},
		{`where req.path like "/b%" and level in ("warn", "error") | stats dc(user) as n`, "n 2"},
		{`where not (status = 200) | stats count`, "count 3"},
		{`eval x = substr(user, 1, 2) + "-" + tostring(status) | where user = "lee" | table x`, "x le-500"},
	}
	for _, c := range cases {
		got := strings.Join(flat(fields(runText(t, c.pipe, jsonLogs...))), " ")
		if got != c.want {
			t.Errorf("%s\n got  %q\n want %q", c.pipe, got, c.want)
		}
	}
}

func TestAnalyzeExpressions(t *testing.T) {
	sh := &Shell{ui: &UI{}, threads: 1}
	cases := []struct {
		line    string
		runCmds int
		badFrom int
	}{
		{"where status >", 0, 13},
		{"grep x | where status > 5", 2, -1},
		{"grep x | where status > ", 1, 22},
		{"grep x | stats count by", 1, 21},
		{"grep x | stats foo(ms)", 1, 15},
		{"grep x | where level = \"err", 1, 23},
		{"stats count by host > out.txt", 0, -1}, // redirection: not previewed
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

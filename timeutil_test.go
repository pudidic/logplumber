package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func utc(t *testing.T) {
	t.Helper()
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
}

func TestFindTime(t *testing.T) {
	utc(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	cases := []struct{ line, want string }{
		{`{"ts":"2026-10-14T09:00:00.555+09:00","level":"warn"}`, "2026-10-14T00:00:00.555Z"},
		{`time=2026-10-14T09:00:03Z level=error`, "2026-10-14T09:00:03Z"},
		{`2026-10-14 09:00:03,250 INFO started`, "2026-10-14T09:00:03.25Z"},
		{`2026/10/14 09:00:03 [error] 12#0: upstream timed out`, "2026-10-14T09:00:03Z"},
		{`192.0.2.1 - - [14/Oct/2026:09:00:00 +0900] "GET / HTTP/1.1" 200 5`, "2026-10-14T00:00:00Z"},
		{`[Wed Oct 14 09:01:48.000000 2026] [core:error] [pid 4344]`, "2026-10-14T09:01:48Z"},
		{`Oct 14 09:00:46 web01 sshd[1]: Accepted`, "2026-10-14T09:00:46Z"},
		{`Oct  4 09:00:46 web01 cron[1]: run`, "2026-10-04T09:00:46Z"},
		{`Dec 31 23:59:59 web01 kernel: late`, "2025-12-31T23:59:59Z"}, // no year: not in the future
		{`2026-10-14T09:00:00-05:00 x`, "2026-10-14T14:00:00Z"},
	}
	for _, c := range cases {
		got, ok := findTime(c.line, now)
		if !ok {
			t.Errorf("%q: no time found", c.line)
			continue
		}
		if s := got.UTC().Format(time.RFC3339Nano); s != c.want {
			t.Errorf("%q: got %s, want %s", c.line, s, c.want)
		}
	}
	for _, line := range []string{"no time here", "version 2026-10-14", "ip 10.1.4.12 port 42", "Octopus 14 09:00:00"} {
		if got, ok := findTime(line, now); ok {
			t.Errorf("%q: unexpected time %v", line, got)
		}
	}
}

func TestEpochField(t *testing.T) {
	l := Line{Text: `{"time":1791936000123,"msg":"x"}`}
	if v, _ := l.Field("_time"); v != "1791936000.123" {
		t.Errorf("epoch ms: got %q", v)
	}
	l = Line{Text: `ts=1791936000 msg=x`}
	if v, _ := l.Field("_time"); v != "1791936000" {
		t.Errorf("epoch s: got %q", v)
	}
	l = Line{Text: `plain text`}
	if _, ok := l.Field("_time"); ok {
		t.Error("plain text has no _time")
	}
}

func TestSpanFloor(t *testing.T) {
	utc(t)
	at := time.Date(2026, 10, 14, 9, 47, 31, 0, time.UTC) // a Wednesday
	cases := []struct{ span, want string }{
		{"30s", "2026-10-14T09:47:30Z"}, {"5m", "2026-10-14T09:45:00Z"}, {"1h", "2026-10-14T09:00:00Z"},
		{"3h", "2026-10-14T09:00:00Z"}, {"1d", "2026-10-14T00:00:00Z"}, {"1w", "2026-10-11T00:00:00Z"},
		{"1mon", "2026-10-01T00:00:00Z"}, {"1q", "2026-10-01T00:00:00Z"}, {"1y", "2026-01-01T00:00:00Z"},
		{"300", "2026-10-14T09:45:00Z"},
	}
	for _, c := range cases {
		sp, err := parseSpan(c.span)
		if err != nil {
			t.Fatalf("%s: %v", c.span, err)
		}
		if got := sp.floor(at).UTC().Format(time.RFC3339); got != c.want {
			t.Errorf("%s: got %s, want %s", c.span, got, c.want)
		}
	}
	for _, bad := range []string{"", "5x", "0m", "m5"} {
		if _, err := parseSpan(bad); err == nil {
			t.Errorf("span %q: expected an error", bad)
		}
	}
	// each automatic span divides the next
	at = time.Date(2026, 10, 14, 23, 59, 59, 0, time.UTC)
	for i := 1; i < len(autoSpans); i++ {
		fine := autoSpans[i-1].floor(at)
		if autoSpans[i].floor(fine) != autoSpans[i].floor(at) {
			t.Errorf("%s does not nest in %s", autoSpans[i-1].text, autoSpans[i].text)
		}
	}
}

func TestParseTimeSpec(t *testing.T) {
	utc(t)
	now := time.Date(2026, 10, 14, 9, 47, 31, 0, time.UTC)
	cases := []struct{ spec, want string }{
		{"now", "2026-10-14T09:47:31Z"}, {"-15m", "2026-10-14T09:32:31Z"}, {"-24h", "2026-10-13T09:47:31Z"},
		{"-1d@d", "2026-10-13T00:00:00Z"}, {"@h", "2026-10-14T09:00:00Z"}, {"@d+9h", "2026-10-14T09:00:00Z"},
		{"@w0", "2026-10-11T00:00:00Z"}, {"@w1", "2026-10-12T00:00:00Z"}, {"-1mon@mon", "2026-09-01T00:00:00Z"},
		{"@y", "2026-01-01T00:00:00Z"}, {"0", "1970-01-01T00:00:00Z"}, {"1791936000", "2026-10-14T00:00:00Z"},
		{"2026-10-14T09:00:00", "2026-10-14T09:00:00Z"}, {"2026-10-14", "2026-10-14T00:00:00Z"},
		{"10/14/2026:09:00:00", "2026-10-14T09:00:00Z"},
	}
	for _, c := range cases {
		got, err := parseTimeSpec(c.spec, now)
		if err != nil {
			t.Errorf("%s: %v", c.spec, err)
			continue
		}
		if s := got.UTC().Format(time.RFC3339); s != c.want {
			t.Errorf("%s: got %s, want %s", c.spec, s, c.want)
		}
	}
	for _, bad := range []string{"-3x", "yesterday", "@zz", "-"} {
		if _, err := parseTimeSpec(bad, now); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestStrftimeStrptime(t *testing.T) {
	utc(t)
	tm := time.Date(2026, 10, 4, 9, 5, 7, 123000000, time.UTC)
	if got := strftime(tm, "%Y-%m-%d %H:%M:%S.%3N %b %a %j %e|%%"); got != "2026-10-04 09:05:07.123 Oct Sun 277  4|%" {
		t.Errorf("strftime: %q", got)
	}
	p, ok := strptime("04/Oct/2026 09:05", "%d/%b/%Y %H:%M")
	if !ok || !p.Equal(time.Date(2026, 10, 4, 9, 5, 0, 0, time.UTC)) {
		t.Errorf("strptime: %v %v", p, ok)
	}
	out := runText(t, `eval d = strftime(_time, "%F %T"), back = strptime(d, "%Y-%m-%d %H:%M:%S") - _time | table d back`,
		"2026-10-14T09:00:00Z ok")
	if len(out) != 2 || strings.Join(fields(out)[1], " ") != "2026-10-14 09:00:00 0" {
		t.Errorf("eval time functions: %q", out)
	}
}

func TestSearchTimeRange(t *testing.T) {
	utc(t)
	in := []string{
		"2020-01-01T00:00:00Z old",
		"2020-01-01T00:59:59Z edge",
		"2020-01-01T01:00:00Z excluded (latest is exclusive)",
		"no timestamp",
	}
	out := runText(t, "search earliest=2020-01-01T00:00:00 latest=2020-01-01T01:00:00", in...)
	if len(out) != 2 || !strings.HasSuffix(out[1], "edge") {
		t.Errorf("search: %q", out)
	}
	out = runText(t, "search earliest=-1h", in...)
	if len(out) != 0 {
		t.Errorf("search earliest=-1h: %q", out)
	}
}

func TestBin(t *testing.T) {
	utc(t)
	out := runText(t, "bin span=5m | stats count by _time | sort +_time",
		"2026-10-14T09:01:00Z a", "2026-10-14T09:04:59Z b", "2026-10-14T09:05:00Z c")
	if strings.Join(strings.Fields(strings.Join(out, " ")), " ") != "_time count 2026-10-14 09:00:00 2 2026-10-14 09:05:00 1" {
		t.Errorf("bin: %q", out)
	}
	out = runText(t, "bin ms span=100 | table ms", "ms=150", "ms=99", "ms=x")
	if got := strings.Join(strings.Fields(strings.Join(out[1:], " ")), " "); got != "100-200 0-100 x" {
		t.Errorf("numeric bin: %q", out)
	}
}

func TestTimechart(t *testing.T) {
	utc(t)
	in := []string{
		"2026-10-14T09:00:10Z level=info ms=10",
		"2026-10-14T09:00:20Z level=error ms=30",
		"2026-10-14T09:12:00Z level=info ms=20",
		"no time level=info",
	}
	out := runText(t, "timechart span=5m count, avg(ms)", in...)
	rows := fields(out)
	// header, 09:00, 09:05 (filled), 09:10
	if len(rows) != 4 {
		t.Fatalf("timechart: %q", out)
	}
	if got := strings.Join(rows[1], " "); got != "2026-10-14 09:00:00 2 20" {
		t.Errorf("first bucket: %q", out[1])
	}
	if got := strings.Join(rows[2], " "); got != "2026-10-14 09:05:00 0" {
		t.Errorf("filled bucket: %q", out[2])
	}
	out = runText(t, "timechart span=5m count by level", in...)
	if !strings.Contains(out[0], "error") || !strings.Contains(out[0], "info") || !strings.Contains(out[0], "▓ info") {
		t.Errorf("split header: %q", out[0])
	}
	if got := strings.Join(fields(out)[1][:4], " "); got != "2026-10-14 09:00:00 1 1" {
		t.Errorf("split row: %q", out[1])
	}
	// limit keeps the biggest series and folds the rest into OTHER
	out = runText(t, "timechart span=1h count by level limit=1", in...)
	if !strings.Contains(out[0], "info") || !strings.Contains(out[0], "OTHER") || strings.Contains(out[0], "error") {
		t.Errorf("limit: %q", out)
	}
	// rows keep numeric fields for later commands
	out = runText(t, "timechart span=5m count | where count > 0 | table _time count", in...)
	if len(out) != 3 {
		t.Errorf("where after timechart: %q", out)
	}
}

func TestTimechartAutoSpan(t *testing.T) {
	utc(t)
	var in []string
	start := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 600; i++ { // one event per second for 10 minutes
		in = append(in, start.Add(time.Duration(i)*time.Second).Format(time.RFC3339)+" x")
	}
	out := runText(t, "timechart count", in...)
	// 600 one-second buckets exceed the limit; 10s gives 60 rows of 10
	if len(out) != 61 || strings.Fields(out[1])[2] != "10" {
		t.Errorf("auto span: %d rows, %q", len(out), out[:2])
	}

	// partial states from worker threads at different spans merge correctly
	c := &statsCfg{tc: &timechartCfg{limit: 10}, by: []string{"_time"}, aggs: []aggSpec{{fn: "count", name: "count"}}}
	spec := statsSpec("timechart", c)
	final := spec.New()
	var parts []Line
	for w := 0; w < 2; w++ {
		p := spec.NewPartial()
		for i := w; i < len(in); i += 2 {
			if w == 1 && i > 100 {
				break // the second worker sees only the first 100 seconds: a finer span
			}
			p.Push(Line{Text: in[i]}, func(l Line) bool { parts = append(parts, l); return true })
		}
		p.Finish(func(l Line) bool { parts = append(parts, l); return true })
	}
	for _, l := range parts {
		final.Push(l, nil)
	}
	total := 0
	final.Finish(func(l Line) bool {
		v, _ := l.Field("count")
		var n int
		fmt.Sscan(v, &n)
		total += n
		return true
	})
	if total != 300+50 {
		t.Errorf("merged count %d, want 350", total)
	}
}

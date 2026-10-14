package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lineFields(t *testing.T, l Line, names ...string) string {
	t.Helper()
	var out []string
	for _, n := range names {
		v, ok := l.Field(n)
		if !ok {
			v = "-"
		}
		out = append(out, n+"="+v)
	}
	return strings.Join(out, " ")
}

func TestBuiltinSourcetypes(t *testing.T) {
	conf.Store(nil)
	cases := []struct{ line, names, want string }{
		{`192.0.2.1 - kim [14/Oct/2026:09:00:00 +0900] "GET /search?q=boots HTTP/1.1" 404 512 "https://x/" "curl/8.9.1" rt=0.25`,
			"sourcetype clientip user method uri_path uri_query version status bytes referer useragent rt ident",
			"sourcetype=access_combined clientip=192.0.2.1 user=kim method=GET uri_path=/search uri_query=q=boots version=HTTP/1.1 status=404 bytes=512 referer=https://x/ useragent=curl/8.9.1 rt=0.25 ident=-"},
		{`10.0.0.1 - - [14/Oct/2026:09:00:00 +0900] "POST /api HTTP/2.0" 201 -`,
			"sourcetype method uri_path status bytes user", "sourcetype=access_combined method=POST uri_path=/api status=201 bytes=- user=-"},
		{`[Wed Oct 14 09:01:48.000000 2026] [proxy_http:error] [pid 4344] [client 198.51.100.23:54589] AH01102: error reading status line`,
			"sourcetype module log_level pid clientip clientport error_code message",
			"sourcetype=apache_error module=proxy_http log_level=error pid=4344 clientip=198.51.100.23 clientport=54589 error_code=AH01102 message=error reading status line"},
		{`[Wed Oct 14 09:01:48 2026] [notice] [pid 1] AH00094: started`,
			"sourcetype module log_level error_code message", "sourcetype=apache_error module=- log_level=notice error_code=AH00094 message=started"},
		{`2026/10/14 09:00:03 [error] 12#0: *5 upstream timed out, client: 192.0.2.1, server: example.com, request: "GET /api HTTP/1.1"`,
			"sourcetype log_level pid message clientip server request",
			"sourcetype=nginx_error log_level=error pid=12 message=upstream timed out clientip=192.0.2.1 server=example.com request=GET /api HTTP/1.1"},
		{`Oct 14 09:00:46 web01 sshd[4303]: Failed password for root from 192.0.2.41 port 33452 ssh2`,
			"sourcetype host process pid message", "sourcetype=syslog host=web01 process=sshd pid=4303 message=Failed password for root from 192.0.2.41 port 33452 ssh2"},
		{`Oct 14 09:00:46 web01 kernel: TCP flood src=10.1.4.12 port=42987`,
			"sourcetype process pid src", "sourcetype=syslog process=kernel pid=- src=10.1.4.12"},
		{`{"level":"info","req":{"path":"/a"}}`, "sourcetype level req.path", "sourcetype=_json level=info req.path=/a"},
		{`time=2026-10-14T09:00:03+09:00 level=error job=x`, "sourcetype level job", "sourcetype=kv level=error job=x"},
		{`just text`, "sourcetype source", "sourcetype=- source=app.log"},
	}
	for _, c := range cases {
		l := Line{Text: c.line, Src: "app.log"}
		if got := lineFields(t, l, strings.Fields(c.names)...); got != c.want {
			t.Errorf("%s\n got  %s\n want %s", c.line, got, c.want)
		}
	}
}

func withConfig(t *testing.T, text string) {
	t.Helper()
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.WriteFile(filepath.Join(dir, ".psh.conf"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Cleanup(func() {
		os.Chdir(old)
		conf.Store(nil)
		typeBySource.Clear()
	})
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigSourcetype(t *testing.T) {
	utc(t)
	withConfig(t, `
[sourcetype:pay]
source = pay*.log
extract = ^(?<ts>\S+ \S+) (?<level>\w+) order (?<order>\d+) amount (?<amount>\d+)
time_field = ts
time_format = %d.%m.%Y %H:%M:%S,%3N
eval = k = amount / 1000
eval = big = if(k >= 2, "y", "n")

[sourcetype:custom]
detect = ^CUSTOM
extract = ^CUSTOM (?<word>\w+)
kv = true

[sourcetype:access_combined]
alias = clientip AS src_ip
eval = class = substr(status, 1, 1) + "xx"
`)
	l := Line{Text: "14.10.2026 09:00:05,759 INFO order 7 amount 2500", Src: "dir/pay-1.log"}
	if got := lineFields(t, l, "sourcetype", "level", "order", "k", "big", "_time"); got != "sourcetype=pay level=INFO order=7 k=2.5 big=y _time=1791968405.759" {
		t.Errorf("custom sourcetype: %s", got)
	}
	l = Line{Text: "14.10.2026 09:00:05,759 INFO order 7 amount 2500", Src: "other.log"}
	if got := lineFields(t, l, "sourcetype", "order"); got != "sourcetype=- order=-" {
		t.Errorf("other file: %s", got)
	}
	l = Line{Text: "CUSTOM hello a=1"}
	if got := lineFields(t, l, "sourcetype", "word", "a"); got != "sourcetype=custom word=hello a=1" {
		t.Errorf("detect: %s", got)
	}
	l = Line{Text: `192.0.2.1 - - [14/Oct/2026:09:00:00 +0900] "GET / HTTP/1.1" 503 5`}
	if got := lineFields(t, l, "src_ip", "class"); got != "src_ip=192.0.2.1 class=5xx" {
		t.Errorf("extended built-in: %s", got)
	}
	// calculated fields and sourcetype are not literal text: no raw-text shortcut
	n, _, _ := parseSearchText(`big=y sourcetype=pay`, 0, true)
	l = Line{Text: "14.10.2026 09:00:05,759 INFO order 7 amount 2500", Src: "pay.log"}
	if !n.match(&searchCtx{l: &l}) {
		t.Error("search on calculated field and sourcetype")
	}
}

func TestMacrosAndSavedSearches(t *testing.T) {
	withConfig(t, "[macro:errors]\ndefinition = level=error OR status>=500\n"+
		"[macro:over(2)]\nargs = f, n\ndefinition = $f$>$n$\n"+
		"[macro:nested]\ndefinition = `errors` `over(ms, 10)`\n"+
		"[search:slow]\nsearch = `over(ms, 1000)` | stats count\n")
	cases := []struct{ in, want string }{
		{"`errors` | head 5", "level=error OR status>=500 | head 5"},
		{"`over(ms, 100)`", "ms>100"},
		{"`nested`", "level=error OR status>=500 ms>10"},
		{"savedsearch slow", "ms>1000 | stats count"},
		{"| savedsearch slow | head 1", "ms>1000 | stats count | head 1"},
		{"no macros here", "no macros here"},
	}
	for _, c := range cases {
		got, err := expandQuery(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"`nope`", "`over(1)`", "savedsearch nope"} {
		if _, err := expandQuery(bad); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	for _, text := range []string{
		"key = value",
		"[nonsense]\n",
		"[sourcetype:x]\nextract = (\n",
		"[sourcetype:x]\nextract = (?<a>.)\n", // neither source= nor detect=
		"[sourcetype:x]\nsource = *.x\nalias = a b\n",
		"[sourcetype:syslog]\nsource = *.log\n",
		"[macro:m(2)]\nargs = a\ndefinition = $a$\n",
		"[search:s]\n",
		"[sourcetype:x]\nsource=*.x\nwhat = 1\n",
	} {
		c := &config{ext: map[string]*sourcetype{}, macros: map[string]macro{}, searches: map[string]string{}, calcFields: map[string]bool{}}
		f, _ := os.CreateTemp(t.TempDir(), "conf")
		f.WriteString(text)
		f.Seek(0, 0)
		if err := c.read("test.conf", f); err == nil {
			t.Errorf("%q: expected an error", text)
		}
		f.Close()
	}
}

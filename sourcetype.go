package main

import (
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Sourcetypes
//
// Every line is assigned a sourcetype, which decides how its fields are
// extracted. Built-in sourcetypes are recognized from the line itself:
//
//	_json            a JSON object (also one that ends the line)
//	access_combined  Apache/nginx access log: clientip ident user method uri uri_path
//	                 uri_query version status bytes referer useragent (+ key=value)
//	apache_error     Apache 2.4 error log: module log_level pid clientip clientport error_code message
//	nginx_error      nginx error log: log_level pid message client server request upstream host
//	syslog           BSD syslog: host process pid message (+ key=value)
//	kv               key=value pairs (logfmt)
//
// .psh.conf can define more sourcetypes (chosen by file name or a line pattern)
// and add field aliases and calculated fields to any sourcetype (see config.go).
// Lines that match none have no sourcetype and no automatic fields.

type sourcetype struct {
	name     string
	sources  []string                                    // file name globs (custom sourcetypes)
	detect   *regexp.Regexp                              // line pattern (custom sourcetypes)
	extracts []*regexp.Regexp                            // named groups become fields
	kv       bool                                        // also extract key=value pairs
	json     bool                                        // the line is (or ends with) a JSON object
	parse    func(text string, m map[string]string) bool // built-in parser: detects and extracts
	post     func(m map[string]string)

	timeField, timeFormat string      // _time from a field, with a strptime format (optional)
	aliases               [][2]string // field, alias
	evals                 []assign    // calculated fields, in order
	builtin               bool
}

var (
	reApacheErr = regexp.MustCompile(`^\[[^\]]+\] \[(?:(?P<module>[^:\]]+):)?(?P<log_level>\w+)\] \[pid (?P<pid>\d+)(?::tid \d+)?\](?: \[client (?P<clientip>[^\]]+?):(?P<clientport>\d+)\])? (?:(?P<error_code>AH\d+): )?(?P<message>.*)$`)
	reNginxErr  = regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d \[(?P<log_level>\w+)\] (?P<pid>\d+)#\d+: (?:\*\d+ )?(?P<message>.*)$`)
	reNginxKV   = regexp.MustCompile(`, (client|server|request|upstream|host|referrer): ("[^"]*"|[^,]*)`)
)

// builtinTypes are tried in order after the custom sourcetypes.
var builtinTypes = []*sourcetype{
	{name: "_json", json: true, builtin: true},
	{name: "access_combined", parse: parseAccess, builtin: true},
	{name: "apache_error", extracts: []*regexp.Regexp{reApacheErr}, builtin: true},
	{name: "nginx_error", extracts: []*regexp.Regexp{reNginxErr}, builtin: true, post: func(m map[string]string) {
		msg := m["message"]
		for _, kv := range reNginxKV.FindAllStringSubmatch(msg, -1) {
			v := kv[2]
			if u, err := strconv.Unquote(v); err == nil {
				v = u
			}
			name := kv[1]
			if name == "client" {
				name = "clientip"
			}
			m[name] = v
		}
		if i := strings.Index(msg, ", client: "); i >= 0 {
			m["message"] = msg[:i]
		}
	}},
	{name: "syslog", parse: parseSyslog, builtin: true},
	{name: "kv", kv: true, builtin: true},
}

// detectBuiltin finds the built-in sourcetype of a line and extracts its fields
// into m.
func detectBuiltin(text string, m map[string]string) *sourcetype {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil
	}
	if strings.HasSuffix(t, "}") && strings.IndexByte(t, '{') >= 0 && json.Valid([]byte(t[strings.IndexByte(t, '{'):])) {
		return builtinTypes[0]
	}
	switch c := t[0]; {
	case c == '[' && strings.Contains(t, "] [pid "):
		if reApacheErr.MatchString(t) {
			return builtinTypes[2]
		}
	case isDigit(c) || c == ':' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F':
		if parseAccess(t, m) {
			return builtinTypes[1]
		}
		if len(t) > 20 && t[4] == '/' && reNginxErr.MatchString(t) {
			return builtinTypes[3]
		}
	}
	if c := t[0]; c >= 'A' && c <= 'Z' && parseSyslog(t, m) {
		return builtinTypes[4]
	}
	if strings.IndexByte(t, '=') > 0 && reKV.MatchString(t) {
		return builtinTypes[5]
	}
	return nil
}

// nextWord returns the text of s up to the first space and the rest after it.
func nextWord(s string) (string, string, bool) {
	i := strings.IndexByte(s, ' ')
	if i <= 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// quoted reads `"..."` (no escapes, as in access logs) and returns the content
// and the rest after it.
func quoted(s string) (string, string, bool) {
	if len(s) < 2 || s[0] != '"' {
		return "", "", false
	}
	i := strings.IndexByte(s[1:], '"')
	if i < 0 {
		return "", "", false
	}
	return s[1 : 1+i], s[2+i:], true
}

// parseAccess reads the Apache/nginx combined log format:
//
//	clientip ident user [time] "method uri version" status bytes ["referer" "useragent"] [key=value...]
func parseAccess(t string, m map[string]string) bool {
	ip, rest, ok := nextWord(t)
	if !ok || strings.Trim(ip, "0123456789abcdefABCDEF.:") != "" {
		return false
	}
	ident, rest, ok1 := nextWord(rest)
	user, rest, ok2 := nextWord(rest)
	if !ok1 || !ok2 || len(rest) == 0 || rest[0] != '[' {
		return false
	}
	i := strings.Index(rest, "] \"")
	if i < 0 {
		return false
	}
	req, rest, ok := quoted(rest[i+2:])
	if !ok || len(rest) < 5 || rest[0] != ' ' {
		return false
	}
	status, rest, _ := strings.Cut(rest[1:], " ")
	if len(status) != 3 || !isDigit(status[0]) || !isDigit(status[1]) || !isDigit(status[2]) {
		return false
	}
	bytes, rest, _ := strings.Cut(rest, " ")
	method, req, _ := strings.Cut(req, " ")
	uri, version, _ := strings.Cut(req, " ")
	if method == "" || uri == "" {
		return false
	}
	m["clientip"], m["method"], m["uri"], m["status"] = ip, method, uri, status
	set := func(k, v string) {
		if v != "" && v != "-" {
			m[k] = v
		}
	}
	set("ident", ident)
	set("user", user)
	set("version", version)
	set("bytes", bytes)
	p, q, has := strings.Cut(uri, "?")
	m["uri_path"] = p
	if has {
		m["uri_query"] = q
	}
	if ref, r2, ok := quoted(rest); ok {
		set("referer", ref)
		if ua, r3, ok := quoted(strings.TrimPrefix(r2, " ")); ok {
			m["useragent"] = ua
			rest = r3
		} else {
			rest = r2
		}
	}
	if strings.IndexByte(rest, '=') > 0 {
		kvFields(rest, m) // nginx extras such as rt=0.25
	}
	return true
}

// parseSyslog reads BSD syslog: "Oct 14 09:00:46 host process[pid]: message".
func parseSyslog(t string, m map[string]string) bool {
	if len(t) < 17 {
		return false
	}
	if _, ok := month(t, 0); !ok || t[3] != ' ' || t[6] != ' ' || t[9] != ':' || t[12] != ':' || t[15] != ' ' {
		return false
	}
	host, rest, ok := nextWord(t[16:])
	if !ok {
		return false
	}
	i := strings.Index(rest, ": ")
	if i <= 0 {
		return false
	}
	proc, msg := rest[:i], rest[i+2:]
	pid := ""
	if j := strings.IndexByte(proc, '['); j > 0 && strings.HasSuffix(proc, "]") {
		proc, pid = proc[:j], proc[j+1:len(proc)-1]
	}
	if proc == "" || strings.ContainsAny(proc, " :") {
		return false
	}
	m["host"], m["process"], m["message"] = host, proc, msg
	if pid != "" {
		m["pid"] = pid
	}
	if strings.IndexByte(msg, '=') > 0 {
		kvFields(msg, m)
	}
	return true
}

// typeBySource caches the custom sourcetype chosen by file name for each source.
var typeBySource sync.Map // src -> *sourcetype (nil value: none)

// lineType returns the custom sourcetype of a line (by file name or line pattern).
func (l *Line) lineType() *sourcetype {
	conf := activeConfig()
	if l.Src != "" && conf.hasSourceRules {
		if v, ok := typeBySource.Load(l.Src); ok {
			if st := v.(*sourcetype); st != nil {
				return conf.extend(st)
			}
		} else {
			var found *sourcetype
			base := path.Base(strings.ReplaceAll(l.Src, "::", "/"))
			for _, st := range conf.types {
				for _, g := range st.sources {
					if ok, _ := path.Match(g, l.Src); ok {
						found = st
					} else if ok, _ := path.Match(g, base); ok {
						found = st
					}
				}
				if found != nil {
					break
				}
			}
			typeBySource.Store(l.Src, found)
			if found != nil {
				return found
			}
		}
	}
	for _, st := range conf.types {
		if st.detect != nil && st.detect.MatchString(l.Text) {
			return st
		}
	}
	return nil
}

// extractFields returns the automatic fields of a line and its sourcetype.
// Calculated fields (eval) are added by the caller, once the fields are visible.
func (l *Line) extractFields() (map[string]string, *sourcetype) {
	m := map[string]string{}
	c := activeConfig()
	st := l.lineType()
	if st == nil {
		if st = c.extend(detectBuiltin(l.Text, m)); st == nil {
			return m, nil
		}
	} else if st.parse != nil {
		st.parse(l.Text, m)
	}
	text := l.Text
	if st.json {
		t := strings.TrimSpace(text)
		if i := strings.IndexByte(t, '{'); i >= 0 {
			dec := json.NewDecoder(strings.NewReader(t[i:]))
			dec.UseNumber()
			var v map[string]any
			if dec.Decode(&v) == nil {
				flattenJSON("", v, m)
			}
		}
	} else if st.kv && st.parse == nil {
		kvFields(text, m)
	}
	for _, re := range st.extracts {
		sm := re.FindStringSubmatch(text)
		if sm == nil {
			continue
		}
		for i, name := range re.SubexpNames() {
			if name != "" && sm[i] != "" {
				m[name] = sm[i]
			}
		}
	}
	if st.post != nil {
		st.post(m)
	}
	m["sourcetype"] = st.name
	for _, a := range st.aliases {
		if v, ok := m[a[0]]; ok {
			m[a[1]] = v
		}
	}
	return m, st
}

// kvFields adds key=value pairs; earlier pairs win.
func kvFields(text string, m map[string]string) {
	for _, kv := range reKV.FindAllStringSubmatch(text, -1) {
		v := kv[2]
		if len(v) >= 2 && v[0] == '"' {
			if u, err := strconv.Unquote(v); err == nil {
				v = u
			} else {
				v = v[1 : len(v)-1]
			}
		}
		if _, dup := m[kv[1]]; !dup {
			m[kv[1]] = v
		}
	}
}

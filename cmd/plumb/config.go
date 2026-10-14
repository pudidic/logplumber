package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

// Configuration: .plumb.conf in the current directory and ~/.config/plumb/plumb.conf
// (the current directory's file wins). INI style; '#' starts a comment.
//
//	[sourcetype:payments]          a new sourcetype, or extend a built-in one by its name
//	source = payments*.log         file name globs (repeatable)
//	detect = ^\d\d\.\d\d\.\d{4}    or a pattern that lines of this type match
//	extract = ^(?<day>\S+) (?<level>\w+) ...   named groups become fields (repeatable)
//	kv = true                      also extract key=value pairs
//	json = true                    lines are JSON
//	time_field = day               _time from this field ...
//	time_format = %d.%m.%Y %H:%M:%S   ... parsed with this strptime format
//	alias = clientip AS src        field alias (repeatable)
//	eval = amount_won = amount * 1 calculated field (repeatable, in order)
//
//	[macro:errors]                 used as `errors` in a query
//	definition = level=error OR log_level=error OR status>=500
//
//	[macro:slow(1)]                with arguments: `slow(1000)`
//	args = ms
//	definition = duration_ms>$ms$
//
//	[search:failed-logins]         run with: savedsearch failed-logins
//	search = "Failed password" | rex "from (?<ip>\S+) port" | top ip

type macro struct {
	args []string
	def  string
}

type config struct {
	files          []string
	types          []*sourcetype          // custom sourcetypes, in file order
	ext            map[string]*sourcetype // built-in sourcetypes with configured additions
	macros         map[string]macro       // by "name" or "name(N)"
	searches       map[string]string
	calcFields     map[string]bool // fields computed by eval (not literal text of _raw)
	hasSourceRules bool
	hasTimeRules   bool
	customExtracts bool // extract= regexes may cut values out of the middle of words
}

var conf atomic.Pointer[config]

func activeConfig() *config {
	if c := conf.Load(); c != nil {
		return c
	}
	return &config{}
}

// extend returns st with its configured additions.
func (c *config) extend(st *sourcetype) *sourcetype {
	if st == nil {
		return nil
	}
	if e := c.ext[st.name]; e != nil {
		return e
	}
	return st
}

// configPaths are the files read, lowest priority first.
func configPaths() []string {
	var ps []string
	if d, err := os.UserConfigDir(); err == nil {
		ps = append(ps, filepath.Join(d, "plumb", "plumb.conf"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		ps = append(ps, filepath.Join(home, ".config", "plumb", "plumb.conf"))
	}
	ps = append(ps, ".plumb.conf")
	seen := map[string]bool{}
	var out []string
	for _, p := range ps {
		if abs, err := filepath.Abs(p); err == nil && !seen[abs] {
			seen[abs] = true
			out = append(out, p)
		}
	}
	return out
}

// loadConfig reads the configuration files and makes them active. Errors name
// the file and line; the files that load are still used.
func loadConfig() error {
	c := &config{ext: map[string]*sourcetype{}, macros: map[string]macro{}, searches: map[string]string{}, calcFields: map[string]bool{}}
	var errs []error
	for _, p := range configPaths() {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		if err := c.read(p, f); err != nil {
			errs = append(errs, err)
		}
		f.Close()
		c.files = append(c.files, p)
	}
	conf.Store(c)
	typeBySource.Clear()
	return errors.Join(errs...)
}

type confSection struct {
	kind, name string
	keys       [][2]string
	file       string
	line       int
}

func (c *config) read(file string, f *os.File) error {
	var secs []*confSection
	var cur *confSection
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			if !strings.HasSuffix(line, "]") {
				return fmt.Errorf("%s:%d: ']' expected", file, n)
			}
			kind, name, ok := strings.Cut(line[1:len(line)-1], ":")
			kind, name = strings.TrimSpace(kind), strings.TrimSpace(name)
			if !ok || name == "" || kind != "sourcetype" && kind != "macro" && kind != "search" {
				return fmt.Errorf("%s:%d: section must be [sourcetype:name], [macro:name] or [search:name]", file, n)
			}
			cur = &confSection{kind: kind, name: name, file: file, line: n}
			secs = append(secs, cur)
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || cur == nil {
			return fmt.Errorf("%s:%d: key = value expected inside a section", file, n)
		}
		cur.keys = append(cur.keys, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	for _, s := range secs {
		var err error
		switch s.kind {
		case "sourcetype":
			err = c.addSourcetype(s)
		case "macro":
			err = c.addMacro(s)
		case "search":
			err = c.addSearch(s)
		}
		if err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *confSection) errf(format string, a ...any) error {
	return fmt.Errorf("%s:%d: [%s:%s]: %s", s.file, s.line, s.kind, s.name, fmt.Sprintf(format, a...))
}

func (c *config) addSourcetype(s *confSection) error {
	var st *sourcetype
	for _, b := range builtinTypes {
		if b.name == s.name {
			cp := *b
			cp.extracts = append([]*regexp.Regexp(nil), b.extracts...)
			st = &cp
		}
	}
	builtin := st != nil
	if !builtin {
		st = &sourcetype{name: s.name}
	}
	for _, kv := range s.keys {
		k, v := kv[0], kv[1]
		switch k {
		case "source":
			if builtin {
				return s.errf("source= is only for new sourcetypes")
			}
			st.sources = append(st.sources, v)
		case "detect":
			if builtin {
				return s.errf("detect= is only for new sourcetypes")
			}
			re, err := regexp.Compile(v)
			if err != nil {
				return s.errf("detect: %v", err)
			}
			st.detect = re
		case "extract":
			re, err := regexp.Compile(v)
			if err != nil {
				return s.errf("extract: %v", err)
			}
			st.extracts = append(st.extracts, re)
			c.customExtracts = true
		case "kv":
			st.kv = v == "true"
		case "json":
			st.json = v == "true"
		case "time_field":
			st.timeField = v
		case "time_format":
			st.timeFormat = v
		case "alias":
			parts := strings.Fields(v)
			if len(parts) != 3 || !strings.EqualFold(parts[1], "as") {
				return s.errf("alias: expected field AS alias")
			}
			st.aliases = append(st.aliases, [2]string{parts[0], parts[2]})
		case "eval":
			name, expr, ok := strings.Cut(v, "=")
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				return s.errf("eval: expected name = expression")
			}
			x, err := parseExpression(strings.TrimSpace(expr), 0)
			if err != nil {
				return s.errf("eval %s: %v", name, err)
			}
			st.evals = append(st.evals, assign{name: name, x: x})
			c.calcFields[name] = true
		default:
			return s.errf("unknown key %q", k)
		}
	}
	if st.timeField != "" || st.timeFormat != "" {
		c.hasTimeRules = true
	}
	if builtin {
		c.ext[st.name] = st
		return nil
	}
	if len(st.sources) == 0 && st.detect == nil {
		return s.errf("a new sourcetype needs source= or detect=")
	}
	c.hasSourceRules = c.hasSourceRules || len(st.sources) > 0
	for i, old := range c.types { // a later file redefines a sourcetype
		if old.name == st.name {
			c.types[i] = st
			return nil
		}
	}
	c.types = append(c.types, st)
	return nil
}

var reMacroName = regexp.MustCompile(`^([A-Za-z_][\w.-]*)(?:\((\d+)\))?$`)

func (c *config) addMacro(s *confSection) error {
	m := reMacroName.FindStringSubmatch(s.name)
	if m == nil {
		return s.errf("invalid macro name (use name or name(N))")
	}
	var mac macro
	for _, kv := range s.keys {
		switch kv[0] {
		case "definition":
			mac.def = kv[1]
		case "args":
			for _, a := range strings.Split(kv[1], ",") {
				if a = strings.TrimSpace(a); a != "" {
					mac.args = append(mac.args, a)
				}
			}
		default:
			return s.errf("unknown key %q", kv[0])
		}
	}
	if mac.def == "" {
		return s.errf("definition= expected")
	}
	if want := m[2]; want != "" && fmt.Sprint(len(mac.args)) != want || want == "" && len(mac.args) > 0 {
		return s.errf("the name says %s argument(s) but args= lists %d", orZero(want), len(mac.args))
	}
	c.macros[s.name] = mac
	return nil
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func (c *config) addSearch(s *confSection) error {
	for _, kv := range s.keys {
		if kv[0] != "search" {
			return s.errf("unknown key %q", kv[0])
		}
		c.searches[s.name] = kv[1]
	}
	if c.searches[s.name] == "" {
		return s.errf("search= expected")
	}
	return nil
}

// ---------------- macros and saved searches ----------------

var reMacroUse = regexp.MustCompile("`([A-Za-z_][\\w.-]*)(?:\\(([^`]*)\\))?`")

// expandQuery replaces `macro` and `macro(args)` and a leading "savedsearch name"
// (also "| savedsearch name") with their definitions.
func expandQuery(q string) (string, error) {
	c := activeConfig()
	t := strings.TrimSpace(q)
	t = strings.TrimSpace(strings.TrimPrefix(t, "|"))
	if rest, ok := strings.CutPrefix(t, "savedsearch "); ok {
		name, tail, _ := strings.Cut(strings.TrimSpace(rest), "|")
		name = strings.TrimSpace(name)
		s, ok := c.searches[name]
		if !ok {
			return q, fmt.Errorf("savedsearch: no saved search %q (see config)", name)
		}
		q = s
		if strings.TrimSpace(tail) != "" {
			q += " |" + tail
		}
	}
	for depth := 0; strings.Contains(q, "`"); depth++ {
		if depth == 10 {
			return q, errors.New("macros nest more than 10 levels deep")
		}
		var err error
		q = reMacroUse.ReplaceAllStringFunc(q, func(use string) string {
			m := reMacroUse.FindStringSubmatch(use)
			var args []string
			key := m[1]
			if strings.Contains(use, "(") {
				for _, a := range strings.Split(m[2], ",") {
					args = append(args, strings.TrimSpace(a))
				}
				key = fmt.Sprintf("%s(%d)", m[1], len(args))
			}
			mac, ok := c.macros[key]
			if !ok {
				if err == nil {
					err = fmt.Errorf("unknown macro `%s` (see config)", key)
				}
				return use
			}
			def := mac.def
			for i, a := range mac.args {
				def = strings.ReplaceAll(def, "$"+a+"$", args[i])
			}
			return def
		})
		if err != nil {
			return q, err
		}
		if !reMacroUse.MatchString(q) {
			break
		}
	}
	return q, nil
}

// describe prints the configuration for the config built-in.
func (c *config) describe() string {
	var b strings.Builder
	if len(c.files) == 0 {
		b.WriteString("no configuration files (looked for " + strings.Join(configPaths(), ", ") + ")\n")
	} else {
		b.WriteString("files: " + strings.Join(c.files, ", ") + "\n")
	}
	b.WriteString("\nsourcetypes (first match wins):\n")
	for _, st := range c.types {
		how := []string{}
		if len(st.sources) > 0 {
			how = append(how, "source="+strings.Join(st.sources, ","))
		}
		if st.detect != nil {
			how = append(how, "detect="+st.detect.String())
		}
		fmt.Fprintf(&b, "  %-16s %s\n", st.name, strings.Join(how, " "))
	}
	for _, st := range builtinTypes {
		extra := ""
		if e := c.ext[st.name]; e != nil {
			extra = fmt.Sprintf(" (+%d alias, %d eval, %d extract)", len(e.aliases), len(e.evals), len(e.extracts)-len(st.extracts))
		}
		fmt.Fprintf(&b, "  %-16s built-in%s\n", st.name, extra)
	}
	if len(c.macros) > 0 {
		b.WriteString("\nmacros:\n")
		for _, k := range sortedNames(c.macros) {
			fmt.Fprintf(&b, "  `%s`  %s\n", k, c.macros[k].def)
		}
	}
	if len(c.searches) > 0 {
		b.WriteString("\nsaved searches (savedsearch name):\n")
		for _, k := range sortedNames(c.searches) {
			fmt.Fprintf(&b, "  %-16s %s\n", k, c.searches[k])
		}
	}
	return b.String()
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

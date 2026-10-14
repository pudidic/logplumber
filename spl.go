package main

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// SPL mode: queries are written in Splunk's Search Processing Language.
//
//	error status>=500 | stats count by req.path | sort -count
//
// The first part of a query is always a search (the word "search" is optional).
// Commands are separated by '|'. Double quotes delimit strings everywhere; single
// quotes are field names inside where/eval. There is no redirection: use outputcsv.

// splCommands are the commands of SPL mode.
var splCommands = map[string]bool{
	"search": true, "where": true, "eval": true, "rex": true, "regex": true, "stats": true,
	"timechart": true, "top": true, "rare": true, "table": true, "fields": true, "rename": true,
	"dedup": true, "sort": true, "head": true, "tail": true, "reverse": true, "bin": true,
	"bucket": true, "fieldsummary": true, "outputcsv": true,
}

// shellCommands are the commands of shell mode.
var shellCommands = map[string]bool{
	"grep": true, "egrep": true, "fgrep": true, "sort": true, "uniq": true, "wc": true, "head": true,
	"tail": true, "cat": true, "cut": true, "tr": true, "sed": true, "find": true, "ls": true, "echo": true,
}

type splSeg struct {
	cmd   rawCmd
	start int   // rune offset of the segment
	err   error // set on the last segment when the line ends inside a quote
}

// splitSPL splits an SPL query into commands. The first segment is the search.
func splitSPL(line string) []splSeg {
	rs := []rune(line)
	var segs []splSeg
	i := 0
	for {
		for i < len(rs) && unicode.IsSpace(rs[i]) {
			i++
		}
		start := i
		// command name
		j := i
		for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '|' && rs[j] != '"' && rs[j] != '\'' && rs[j] != '(' {
			j++
		}
		name := string(rs[i:j])
		implicit := len(segs) == 0 && name != "search"
		search := implicit || name == "search"
		rawFrom := j
		if implicit {
			name, rawFrom = "search", start
		}
		// the rest of the segment, up to '|' outside quotes
		var q rune
		qpos := -1
		k := rawFrom
		for ; k < len(rs); k++ {
			c := rs[k]
			if q != 0 {
				if c == '\\' && k+1 < len(rs) {
					k++
				} else if c == q {
					q = 0
				}
				continue
			}
			if c == '"' || c == '\'' && !search {
				q, qpos = c, k
				continue
			}
			if c == '|' {
				break
			}
		}
		rawStart := rawFrom
		for rawStart < k && unicode.IsSpace(rs[rawStart]) {
			rawStart++
		}
		raw := strings.TrimRightFunc(string(rs[rawStart:k]), unicode.IsSpace)
		seg := splSeg{start: start, cmd: rawCmd{args: []word{{s: name, pos: start}}}}
		if raw != "" {
			seg.cmd.args = append(seg.cmd.args, word{s: raw, raw: true, quoted: true, pos: rawStart})
		}
		empty := raw == "" && (implicit || name == "")
		switch {
		case q != 0:
			seg.err = errAt(qpos, "unterminated quote")
		case empty && len(segs) == 0 && k < len(rs):
			seg.start, seg.err = k, errAt(k, "a search must come before '|' (e.g. * | stats count)")
		case empty && len(segs) > 0:
			seg.start = max(0, start-1)
			seg.err = errAt(seg.start, "command expected after '|'")
		}
		if !empty || seg.err != nil {
			segs = append(segs, seg)
		}
		if seg.err != nil || k >= len(rs) {
			return segs
		}
		i = k + 1
	}
}

// splParse parses an SPL query into a pipeline.
func splParse(line string) (*pipelineAST, error) {
	segs := splitSPL(line)
	if len(segs) == 0 {
		return nil, errors.New("empty query")
	}
	ast := &pipelineAST{spl: true}
	for _, s := range segs {
		if s.err != nil {
			return nil, s.err
		}
		ast.cmds = append(ast.cmds, s.cmd)
	}
	return ast, nil
}

// splWords splits command arguments into words; double quotes group words.
func splWords(src string, base int) []word {
	rs := []rune(src)
	var out []word
	for i := 0; i < len(rs); {
		if unicode.IsSpace(rs[i]) || rs[i] == ',' {
			i++
			continue
		}
		var b strings.Builder
		start, quoted := i, false
		for i < len(rs) && !unicode.IsSpace(rs[i]) && rs[i] != ',' {
			if rs[i] == '"' {
				quoted = true
				i++
				for i < len(rs) && rs[i] != '"' {
					if rs[i] == '\\' && i+1 < len(rs) {
						i++
					}
					b.WriteRune(rs[i])
					i++
				}
				i++
				continue
			}
			b.WriteRune(rs[i])
			i++
		}
		out = append(out, word{s: b.String(), quoted: quoted, pos: base + start})
	}
	return out
}

// buildSPLSpec parses one SPL command. The first command's source= terms become
// file operands.
func buildSPLSpec(cmd rawCmd, first bool) (*Spec, []word, error) {
	name := cmd.args[0].s
	src, pos := rawArg(cmd.args)
	var s *Spec
	var err error
	switch name {
	case "search":
		var sources []string
		s, sources, err = parseSearchCmd(src, pos, first)
		var ops []word
		for _, p := range sources {
			ops = append(ops, word{s: p, nameOnly: !strings.ContainsAny(p, `/\`)})
		}
		return s, ops, err
	case "where":
		s, err = parseWhere(cmd.args)
	case "eval":
		s, err = parseEval(cmd.args)
	case "rex":
		s, err = parseRex(cmd.args)
	case "stats":
		s, err = parseStats(cmd.args)
	case "top", "rare":
		s, err = parseTop(name, cmd.args)
	case "table":
		s, err = parseTable(cmd.args)
	case "fieldsummary":
		s, err = parseFieldSummary(cmd.args)
	case "timechart":
		s, err = parseTimechart(cmd.args)
	case "bin", "bucket":
		s, err = parseBin(name, splWords(src, pos))
	case "head", "tail":
		s, err = parseSPLHeadTail(name, splWords(src, pos), pos)
	case "sort":
		s, err = parseSPLSort(splWords(src, pos), pos)
	case "dedup":
		s, err = parseDedup(splWords(src, pos), pos)
	case "fields":
		s, err = parseFields(splWords(src, pos), pos)
	case "rename":
		s, err = parseRename(splWords(src, pos), pos)
	case "reverse":
		if src != "" {
			return nil, nil, errAt(pos, "reverse: takes no arguments")
		}
		s = &Spec{Name: "reverse", Blocking: true, New: func() Stage { return &reverseStage{} }}
	case "regex":
		s, err = parseRegex(cmd.args)
	case "outputcsv":
		s, err = parseOutputCSV(splWords(src, pos), pos)
	default:
		if shellCommands[name] {
			return nil, nil, errAt(cmd.args[0].pos, "%s is a shell command — press Tab or type sh for shell mode", name)
		}
		return nil, nil, errAt(cmd.args[0].pos, "%s: unknown SPL command (see help)", name)
	}
	return s, nil, err
}

// streamingSPL are SPL commands that keep the order of events; orderSPL need
// events in time order; transformingSPL make their own order.
var (
	streamingSPL    = map[string]bool{"search": true, "where": true, "eval": true, "rex": true, "regex": true, "fields": true, "rename": true, "bin": true, "bucket": true}
	transformingSPL = map[string]bool{"stats": true, "timechart": true, "top": true, "rare": true, "fieldsummary": true, "sort": true}
	tableSPL        = map[string]bool{"stats": true, "timechart": true, "top": true, "rare": true, "fieldsummary": true, "table": true}
	modifyingSPL    = map[string]bool{"eval": true, "rex": true, "rename": true, "fields": true, "bin": true, "bucket": true}
)

// liveEventLimit caps the events kept for a type-ahead preview of a search.
const liveEventLimit = 1000

// orderSPL adds what SPL does implicitly: events come newest first (as in
// Splunk, a search returns events in reverse time order) with a timeline of
// all of them, and tables are redrawn when a command changes their fields.
func orderSPL(specs []*Spec, live bool, rt *followCfg) ([]*Spec, *timeline) {
	j := 1
	for j < len(specs) && streamingSPL[specs[j].Name] {
		j++
	}
	if rt != nil && rt.window > 0 { // a sliding window before the first command that needs it
		specs = append(specs[:j], append([]*Spec{windowSpec(rt.window)}, specs[j:]...)...)
		j++
	}
	// Real-time events print as they arrive, unless type-ahead shows the newest.
	streamRT := rt != nil && rt.window == 0 && !live
	if !streamRT && (j == len(specs) || !transformingSPL[specs[j].Name]) {
		limit := 0
		if j < len(specs) && specs[j].Name == "head" {
			limit = specs[j].limit
		} else if live {
			limit = liveEventLimit
		}
		s := newFieldSort([]sortTerm{{field: "_time", desc: true}}, limit)
		add := []*Spec{s}
		var tl *timeline
		if rt == nil { // an event search: count all its events for the timeline
			tl = newTimeline()
			add = []*Spec{timelineSpec(tl), s}
		}
		specs = append(specs[:j], append(add, specs[j:]...)...)
		return retableSPL(specs), tl
	}
	return retableSPL(specs), nil
}

// retableSPL appends a stage that redraws result tables when a later command
// changed their fields.
func retableSPL(specs []*Spec) []*Spec {
	table := false
	for _, s := range specs {
		if tableSPL[s.Name] {
			table = true
		} else if table && modifyingSPL[s.Name] {
			return append(specs, &Spec{Name: "retable", Blocking: true, New: func() Stage { return &retableStage{} }})
		}
	}
	return specs
}

// ---------------- head / tail ----------------

func parseSPLHeadTail(name string, args []word, pos int) (*Spec, error) {
	n := 10
	for _, a := range args {
		v := strings.TrimPrefix(strings.ToLower(a.s), "limit=")
		x, err := strconv.Atoi(v)
		if err != nil || x < 0 {
			return nil, errAt(a.pos, "%s: number expected, e.g. %s 20", name, name)
		}
		n = x
	}
	if len(args) > 1 {
		return nil, errAt(args[1].pos, "%s: one number expected", name)
	}
	if name == "head" {
		return &Spec{Name: "head", limit: n, New: func() Stage { return &headStage{n: n} }}, nil
	}
	return &Spec{Name: "tail", Blocking: true, New: func() Stage { return &tailStage{n: n} }}, nil
}

// ---------------- sort ----------------

func newFieldSort(terms []sortTerm, limit int) *Spec {
	return &Spec{Name: "sort", Blocking: true, New: func() Stage { return &fieldSortStage{terms: terms, limit: limit} }}
}

var reSortFunc = regexp.MustCompile(`^(?:num|str|ip|auto)\((.+)\)$`)

// parseSPLSort parses "sort [N] [limit=N] [-|+]field, ..." (ascending by default).
func parseSPLSort(args []word, pos int) (*Spec, error) {
	var terms []sortTerm
	limit := 0
	desc := false
	for i, a := range args {
		v := a.s
		switch {
		case i == 0 && isAllDigits(v):
			limit, _ = strconv.Atoi(v)
		case strings.HasPrefix(strings.ToLower(v), "limit="):
			n, err := strconv.Atoi(v[6:])
			if err != nil || n < 0 {
				return nil, errAt(a.pos, "sort: number expected after limit=")
			}
			limit = n
		case v == "-":
			desc = true
		case v == "+":
			desc = false
		default:
			d := desc || strings.HasPrefix(v, "-")
			f := strings.TrimLeft(v, "+-")
			if m := reSortFunc.FindStringSubmatch(f); m != nil {
				f = m[1]
			}
			if f == "" {
				return nil, errAt(a.pos, "sort: field name expected")
			}
			terms = append(terms, sortTerm{field: f, desc: d})
			desc = false
		}
	}
	if len(terms) == 0 {
		return nil, errAt(pos, "sort: field expected, e.g. sort -count")
	}
	return newFieldSort(terms, limit), nil
}

// ---------------- dedup ----------------

type dedupStage struct {
	fields    []string
	n         int
	keepEmpty bool
	consec    bool
	seen      map[string]int
	last      string
}

func (s *dedupStage) Push(l Line, emit Emit) bool {
	vals := make([]string, len(s.fields))
	for i, f := range s.fields {
		v, ok := l.Field(f)
		if !ok {
			if s.keepEmpty {
				return emit(l)
			}
			return true
		}
		vals[i] = v
	}
	k := strings.Join(vals, "\x00")
	if s.consec {
		if k == s.last {
			return true
		}
		s.last = k
		return emit(l)
	}
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	if s.seen[k] >= s.n {
		return true
	}
	s.seen[k]++
	return emit(l)
}
func (s *dedupStage) Finish(Emit) {}

// parseDedup parses "dedup [N] field... [keepempty=true] [consecutive=true]".
func parseDedup(args []word, pos int) (*Spec, error) {
	c := dedupStage{n: 1}
	for i, a := range args {
		k, v, kv := strings.Cut(strings.ToLower(a.s), "=")
		switch {
		case i == 0 && isAllDigits(a.s):
			c.n, _ = strconv.Atoi(a.s)
		case kv && k == "keepempty":
			c.keepEmpty = v == "true" || v == "t"
		case kv && k == "consecutive":
			c.consec = v == "true" || v == "t"
		case kv:
			return nil, errAt(a.pos, "dedup: unknown option %s", a.s)
		default:
			c.fields = append(c.fields, a.s)
		}
	}
	if len(c.fields) == 0 {
		return nil, errAt(pos, "dedup: field expected, e.g. dedup user")
	}
	return &Spec{Name: "dedup", New: func() Stage { d := c; return &d }}, nil
}

// ---------------- fields / rename ----------------

// wildMatch matches a field name against a pattern where '*' matches any run.
func wildMatch(pat, s string) bool {
	if !strings.Contains(pat, "*") {
		return pat == s
	}
	parts := strings.Split(pat, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i, p := range parts[1:] {
		if i == len(parts)-2 {
			return strings.HasSuffix(s, p)
		}
		j := strings.Index(s, p)
		if j < 0 {
			return false
		}
		s = s[j+len(p):]
	}
	return true
}

func matchFieldPats(pats []string, s string) bool {
	for _, p := range pats {
		if wildMatch(p, s) {
			return true
		}
	}
	return false
}

type fieldsStage struct {
	pats []string
	drop bool
}

func (s fieldsStage) Push(l Line, emit Emit) bool {
	l.Field("_") // extract automatic fields before filtering them
	r := l.rec()
	for k := range r.set {
		if !strings.HasPrefix(k, "_") && matchFieldPats(s.pats, k) == s.drop {
			delete(r.set, k)
		}
	}
	r.filtered = true
	if s.drop {
		r.drop = append(r.drop, s.pats...)
	} else {
		r.keep = append([]string(nil), s.pats...)
	}
	return emit(l)
}
func (fieldsStage) Finish(Emit) {}

// parseFields parses "fields [+|-] field, ..." (wildcards allowed).
func parseFields(args []word, pos int) (*Spec, error) {
	s := fieldsStage{}
	for i, a := range args {
		if i == 0 && (a.s == "-" || a.s == "+") {
			s.drop = a.s == "-"
			continue
		}
		s.pats = append(s.pats, a.s)
	}
	if len(s.pats) == 0 {
		return nil, errAt(pos, "fields: field expected, e.g. fields host, status or fields - _raw")
	}
	return &Spec{Name: "fields", Parallel: true, New: func() Stage { return s }}, nil
}

type renameStage struct{ from, to []string }

func (s renameStage) Push(l Line, emit Emit) bool {
	for i, f := range s.from {
		v, ok := l.Field(f)
		if !ok {
			continue
		}
		l.SetField(s.to[i], v)
		r := l.Rec
		delete(r.set, f)
		if r.hidden == nil {
			r.hidden = map[string]bool{}
		}
		if r.renamed == nil {
			r.renamed = map[string]string{}
		}
		r.hidden[f], r.renamed[f], r.filtered = true, s.to[i], true
	}
	return emit(l)
}
func (renameStage) Finish(Emit) {}

// parseRename parses "rename old AS new, ...".
func parseRename(args []word, pos int) (*Spec, error) {
	var s renameStage
	for i := 0; i < len(args); i += 3 {
		if i+2 >= len(args) || !strings.EqualFold(args[i+1].s, "as") {
			p := pos
			if i < len(args) {
				p = args[i].pos
			}
			return nil, errAt(p, "rename: expected old AS new, e.g. rename count AS n")
		}
		s.from = append(s.from, args[i].s)
		s.to = append(s.to, args[i+2].s)
	}
	if len(s.from) == 0 {
		return nil, errAt(pos, "rename: expected old AS new, e.g. rename count AS n")
	}
	return &Spec{Name: "rename", Parallel: true, New: func() Stage { return s }}, nil
}

// ---------------- reverse ----------------

type reverseStage struct{ lines []Line }

func (s *reverseStage) Push(l Line, emit Emit) bool {
	s.lines = append(s.lines, l)
	return true
}

func (s *reverseStage) emitAll(emit Emit) {
	for i := len(s.lines) - 1; i >= 0; i-- {
		if !emit(s.lines[i]) {
			return
		}
	}
}
func (s *reverseStage) Snapshot(emit Emit) { s.emitAll(emit) }
func (s *reverseStage) Finish(emit Emit)   { s.emitAll(emit) }

// ---------------- regex ----------------

type regexStage struct {
	field string
	re    *regexp.Regexp
	not   bool
}

func (s regexStage) Push(l Line, emit Emit) bool {
	v, ok := l.Field(s.field)
	if ok && s.re.MatchString(v) != s.not {
		return emit(l)
	}
	return true
}
func (regexStage) Finish(Emit) {}

// parseRegex parses `regex [field=|field!=]"re"`.
func parseRegex(args []word) (*Spec, error) {
	src, pos := rawArg(args)
	toks, err := lexExpr(src, pos)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks}
	s := regexStage{field: "_raw"}
	if t := p.peek(); (t.kind == tIdent || t.kind == tField) && len(toks) > 2 && toks[1].kind == tOp {
		p.next()
		op := p.next()
		if op.s != "=" && op.s != "!=" {
			return nil, errAt(op.pos, "regex: '=' or '!=' expected")
		}
		s.field, s.not = t.s, op.s == "!="
	}
	t := p.next()
	if t.kind != tStr {
		return nil, errAt(t.pos, `regex: quoted regex expected, e.g. regex _raw="\d{3}"`)
	}
	if s.re, err = regexp.Compile(t.s); err != nil {
		return nil, errAt(t.pos, "regex: invalid regex: %v", err)
	}
	if p.peek().kind != tEOF {
		return nil, p.errExpected("end of regex")
	}
	return &Spec{Name: "regex", Parallel: true, New: func() Stage { return s }}, nil
}

// ---------------- outputcsv ----------------

type outputCSVStage struct {
	path   string
	f      *os.File
	bw     *bufio.Writer
	w      *csv.Writer
	cols   []string
	failed bool
}

func (s *outputCSVStage) Push(l Line, emit Emit) bool {
	if s.w == nil && !s.failed {
		f, err := os.Create(s.path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "plumb: outputcsv:", err)
			s.failed = true
		} else {
			s.f, s.bw = f, bufio.NewWriter(f)
			s.w = csv.NewWriter(s.bw)
			s.cols = []string{"_time", "_src", "_raw"}
			if h := lineHeader(l); h != nil {
				s.cols = h.cols
			}
			s.w.Write(s.cols)
		}
	}
	if s.w != nil {
		rec := make([]string, len(s.cols))
		for i, c := range s.cols {
			rec[i], _ = l.Field(c)
		}
		s.w.Write(rec)
	}
	return emit(l)
}

func (s *outputCSVStage) Finish(Emit) {
	if s.w == nil && !s.failed { // no results: an empty file
		if f, err := os.Create(s.path); err == nil {
			f.Close()
		}
		return
	}
	if s.w != nil {
		s.w.Flush()
		s.bw.Flush()
		s.f.Close()
	}
}

// parseOutputCSV parses "outputcsv file"; ".csv" is added when the name has no extension.
func parseOutputCSV(args []word, pos int) (*Spec, error) {
	if len(args) != 1 {
		return nil, errAt(pos, "outputcsv: file name expected, e.g. outputcsv errors.csv")
	}
	path := args[0].s
	if filepath.Ext(path) == "" {
		path += ".csv"
	}
	return &Spec{Name: "outputcsv", out: path, New: func() Stage { return &outputCSVStage{path: path} }}, nil
}

// ---------------- retable ----------------

// retableStage redraws result tables after eval, rename or fields changed their
// fields; other lines pass through.
type retableStage struct{ lines []Line }

func (s *retableStage) Push(l Line, emit Emit) bool {
	s.lines = append(s.lines, l)
	return true
}

func (s *retableStage) emitAll(emit Emit) {
	var cols []string
	have := map[string]bool{}
	add := func(c string) {
		if !have[c] {
			have[c] = true
			cols = append(cols, c)
		}
	}
	var keep []string
	var rows []Line
	for _, l := range s.lines {
		h := lineHeader(l)
		if h == nil {
			if !emit(l) {
				return
			}
			continue
		}
		r := l.Rec
		for _, c := range h.cols {
			if n, ok := r.renamed[c]; ok {
				add(n)
			} else if r.visible(c) {
				add(c)
			}
		}
		for _, c := range r.order {
			if r.visible(c) {
				add(c)
			}
		}
		if r.keep != nil {
			keep = r.keep
		}
		rows = append(rows, l)
	}
	if len(rows) == 0 {
		return
	}
	if keep != nil { // "fields a, b" also orders the columns
		var ordered []string
		seen := map[string]bool{}
		for _, p := range keep {
			for _, c := range cols {
				if !seen[c] && wildMatch(p, c) {
					seen[c] = true
					ordered = append(ordered, c)
				}
			}
		}
		cols = ordered
	}
	vals := make([][]string, len(rows))
	for i, l := range rows {
		vals[i] = make([]string, len(cols))
		for j, c := range cols {
			vals[i][j], _ = l.Field(c)
		}
	}
	makeTable(cols, vals, emit)
}

func (s *retableStage) Snapshot(emit Emit) { s.emitAll(emit) }
func (s *retableStage) Finish(emit Emit)   { s.emitAll(emit) }

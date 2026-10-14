package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Field commands: rex, where, eval, stats, top, rare, table, fieldsummary, and the
// field form of sort ("sort -count"). Their argument text arrives unparsed (raw).

func rawArg(args []word) (string, int) {
	if len(args) > 1 && args[1].raw {
		return args[1].s, args[1].pos
	}
	end := args[0].pos + len([]rune(args[0].s)) + 1
	return "", end
}

// ---------------- rex ----------------

type rexStage struct {
	field string
	re    *regexp.Regexp
	names []string
}

func (s *rexStage) Push(l Line, emit Emit) bool {
	src, ok := l.Field(s.field)
	if !ok {
		return emit(l)
	}
	m := s.re.FindStringSubmatch(src)
	for i, name := range s.names {
		if name != "" && i < len(m) && m != nil {
			l.SetField(name, m[i])
		}
	}
	return emit(l)
}
func (s *rexStage) Finish(Emit) {}

func parseRex(args []word) (*Spec, error) {
	src, pos := rawArg(args)
	toks, err := lexExpr(src, pos)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks}
	field := "_raw"
	if p.isKw("field") {
		p.next()
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		t := p.next()
		if t.kind != tIdent && t.kind != tField {
			return nil, errAt(t.pos, "rex: field name expected")
		}
		field = t.s
	}
	t := p.next()
	if t.kind != tStr && t.kind != tField {
		return nil, errAt(t.pos, `rex: quoted regex expected, e.g. rex "(?<status>\d{3})"`)
	}
	re, err := regexp.Compile(t.s)
	if err != nil {
		return nil, errAt(t.pos, "rex: invalid regex: %v", err)
	}
	if p.peek().kind != tEOF {
		return nil, p.errExpected("end of rex")
	}
	named := false
	for _, n := range re.SubexpNames() {
		named = named || n != ""
	}
	if !named {
		return nil, errAt(t.pos, "rex: regex needs a named group such as (?<name>...)")
	}
	s := &rexStage{field: field, re: re, names: re.SubexpNames()}
	return &Spec{Name: "rex", Parallel: true, New: func() Stage { return s }}, nil
}

// ---------------- where / eval ----------------

type whereStage struct{ cond node }

func (s whereStage) Push(l Line, emit Emit) bool {
	if s.cond.eval(&l).truthy() {
		return emit(l)
	}
	return true
}
func (whereStage) Finish(Emit) {}

func parseWhere(args []word) (*Spec, error) {
	src, pos := rawArg(args)
	if strings.TrimSpace(src) == "" {
		return nil, errAt(pos, "where: condition expected, e.g. where status >= 500")
	}
	n, err := parseExpression(src, pos)
	if err != nil {
		return nil, err
	}
	return &Spec{Name: "where", Parallel: true, New: func() Stage { return whereStage{cond: n} }}, nil
}

type assign struct {
	name string
	x    node
}

type evalStage struct{ as []assign }

func (s evalStage) Push(l Line, emit Emit) bool {
	for _, a := range s.as {
		v := a.x.eval(&l)
		l.SetField(a.name, v.String())
	}
	return emit(l)
}
func (evalStage) Finish(Emit) {}

func parseEval(args []word) (*Spec, error) {
	src, pos := rawArg(args)
	toks, err := lexExpr(src, pos)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks}
	var as []assign
	for {
		t := p.next()
		if t.kind != tIdent && t.kind != tField {
			return nil, errAt(t.pos, "eval: field name expected, e.g. eval sec = ms / 1000")
		}
		if !p.isOp("=") {
			return nil, p.errExpected("'='")
		}
		p.next()
		x, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		as = append(as, assign{name: t.s, x: x})
		if p.isOp(",") {
			p.next()
			continue
		}
		if p.peek().kind != tEOF {
			return nil, p.errExpected("',' or end of eval")
		}
		break
	}
	return &Spec{Name: "eval", Parallel: true, New: func() Stage { return evalStage{as: as} }}, nil
}

// ---------------- stats ----------------

type aggSpec struct {
	fn    string // count, dc, sum, avg, min, max, perc, values, first, last
	field string
	pct   float64
	name  string // output column
}

type statsCfg struct {
	aggs []aggSpec
	by   []string
	// top/rare
	top      bool
	rare     bool
	limit    int
	topField []string
	tc       *timechartCfg // timechart: by[0] is the time bucket
}

type aggVal struct {
	count    int64
	sum      float64
	nnum     int64
	min, max float64
	nums     []float64
	set      map[string]struct{}
	first    string
	hasFirst bool
	last     string
}

type group struct {
	keys []string
	vals []aggVal
}

// statsState is a mergeable aggregation; worker threads send partial states.
type statsState struct {
	groups map[string]*group
	total  int64
	// timechart without span: index into autoSpans, and the distinct time buckets
	auto    int
	buckets map[string]struct{}
}

func newStatsState() *statsState { return &statsState{groups: map[string]*group{}} }

const maxValues = 100

func (st *statsState) add(c *statsCfg, l *Line) {
	keys := make([]string, len(c.by))
	for i, f := range c.by {
		if i == 0 && c.tc != nil {
			ms, ok := l.lineTime()
			if !ok {
				return
			}
			keys[0] = strconv.FormatInt(st.span(c).floor(time.UnixMilli(ms)).Unix(), 10)
			continue
		}
		v, ok := l.Field(f)
		if !ok {
			return // like the original: events without a by-field are skipped
		}
		keys[i] = v
	}
	k := strings.Join(keys, "\x00")
	g := st.groups[k]
	if g == nil {
		g = &group{keys: keys, vals: make([]aggVal, len(c.aggs))}
		st.groups[k] = g
		if st.newGroup(c, g) {
			st.checkBuckets(c)
			g = st.groups[strings.Join(g.keys, "\x00")]
		}
	}
	st.total++
	for i, a := range c.aggs {
		av := &g.vals[i]
		if a.field == "" {
			av.count++
			continue
		}
		v, ok := l.Field(a.field)
		if !ok {
			continue
		}
		av.count++
		switch a.fn {
		case "dc", "values":
			if av.set == nil {
				av.set = map[string]struct{}{}
			}
			if a.fn == "dc" || len(av.set) < maxValues {
				av.set[v] = struct{}{}
			}
		case "first", "last":
			if !av.hasFirst {
				av.first, av.hasFirst = v, true
			}
			av.last = v
		default:
			f, ok := parseNum(v)
			if !ok {
				continue
			}
			if av.nnum == 0 || f < av.min {
				av.min = f
			}
			if av.nnum == 0 || f > av.max {
				av.max = f
			}
			av.nnum++
			av.sum += f
			if a.fn == "perc" {
				av.nums = append(av.nums, f)
			}
		}
	}
}

func (st *statsState) merge(c *statsCfg, o *statsState) {
	if c.tc != nil && c.tc.span == nil {
		switch { // bring both to the coarser automatic span
		case o.auto < st.auto:
			o.coarsen(c, st.auto)
		case o.auto > st.auto:
			st.coarsen(c, o.auto)
		}
	}
	st.total += o.total
	for k, og := range o.groups {
		g := st.groups[k]
		if g == nil {
			st.groups[k] = og
			st.newGroup(c, og)
			continue
		}
		for i := range c.aggs {
			mergeAgg(c.aggs[i].fn, &g.vals[i], &og.vals[i])
		}
	}
	st.checkBuckets(c)
}

// mergeAgg adds the partial aggregation b to a.
func mergeAgg(fn string, a, b *aggVal) {
	a.count += b.count
	if b.nnum > 0 {
		if a.nnum == 0 || b.min < a.min {
			a.min = b.min
		}
		if a.nnum == 0 || b.max > a.max {
			a.max = b.max
		}
	}
	a.nnum += b.nnum
	a.sum += b.sum
	a.nums = append(a.nums, b.nums...)
	for v := range b.set {
		if a.set == nil {
			a.set = map[string]struct{}{}
		}
		if fn == "dc" || len(a.set) < maxValues {
			a.set[v] = struct{}{}
		}
	}
	if !a.hasFirst && b.hasFirst {
		a.first, a.hasFirst = b.first, true
	}
	if b.hasFirst {
		a.last = b.last
	}
}

func percentile(nums []float64, p float64) float64 {
	if len(nums) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), nums...)
	sort.Float64s(s)
	r := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(r))
	hi := int(math.Ceil(r))
	return s[lo] + (s[hi]-s[lo])*(r-float64(lo))
}

func (a *aggVal) result(spec aggSpec) string {
	switch spec.fn {
	case "count":
		return strconv.FormatInt(a.count, 10)
	case "dc":
		return strconv.Itoa(len(a.set))
	case "values":
		return strings.Join(sortedKeys(a.set), ", ")
	case "first":
		return a.first
	case "last":
		return a.last
	}
	if a.nnum == 0 {
		return ""
	}
	switch spec.fn {
	case "sum":
		return formatNum(a.sum)
	case "avg":
		return formatNum(a.sum / float64(a.nnum))
	case "min":
		return formatNum(a.min)
	case "max":
		return formatNum(a.max)
	case "perc":
		return formatNum(percentile(a.nums, spec.pct))
	}
	return ""
}

type statsStage struct {
	c         *statsCfg
	st        *statsState
	partial   bool // worker-side instance: periodically sends its state downstream
	pending   int
	lastFlush time.Time
}

func (s *statsStage) state() *statsState {
	if s.st == nil {
		s.st = newStatsState()
	}
	return s.st
}

func (s *statsStage) Push(l Line, emit Emit) bool {
	if l.Agg != nil {
		s.state().merge(s.c, l.Agg)
		return true
	}
	s.state().add(s.c, &l)
	if s.partial {
		s.pending++
		if s.pending >= 50000 || s.pending&1023 == 0 && time.Since(s.lastFlush) > 200*time.Millisecond {
			return s.flushPartial(emit)
		}
	}
	return true
}

func (s *statsStage) flushPartial(emit Emit) bool {
	if s.st == nil || s.pending == 0 {
		return true
	}
	st := s.st
	s.st, s.pending, s.lastFlush = nil, 0, time.Now()
	return emit(Line{Agg: st})
}

func (s *statsStage) EndFile(_ string, emit Emit) bool {
	if s.pending > 0 && time.Since(s.lastFlush) > 200*time.Millisecond {
		return s.flushPartial(emit)
	}
	return true
}

func (s *statsStage) emitRows(emit Emit) {
	c := s.c
	st := s.state()
	groups := make([]*group, 0, len(st.groups))
	for _, g := range st.groups {
		groups = append(groups, g)
	}
	if c.top || c.rare {
		s.emitTop(groups, emit)
		return
	}
	if c.tc != nil {
		s.emitTimechart(groups, emit)
		return
	}
	// Biggest first by the first aggregation, then by group key.
	vals := make(map[*group]string, len(groups))
	for _, g := range groups {
		vals[g] = g.vals[0].result(c.aggs[0])
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if r := compareValues(vals[groups[i]], vals[groups[j]]); r != 0 {
			return r > 0
		}
		return strings.Join(groups[i].keys, "\x00") < strings.Join(groups[j].keys, "\x00")
	})
	cols := append([]string(nil), c.by...)
	for _, a := range c.aggs {
		cols = append(cols, a.name)
	}
	rows := make([][]string, len(groups))
	for i, g := range groups {
		row := append([]string(nil), g.keys...)
		for j, a := range c.aggs {
			row = append(row, g.vals[j].result(a))
		}
		rows[i] = row
	}
	makeTable(cols, rows, emit)
}

// emitTop renders top/rare: the most (least) common values with count and percent,
// limited per by-group.
func (s *statsStage) emitTop(groups []*group, emit Emit) {
	c := s.c
	nb := len(c.by) - len(c.topField) // leading keys are the by-clause
	totals := map[string]int64{}
	for _, g := range groups {
		totals[strings.Join(g.keys[:nb], "\x00")] += g.vals[0].count
	}
	sort.SliceStable(groups, func(i, j int) bool {
		bi, bj := strings.Join(groups[i].keys[:nb], "\x00"), strings.Join(groups[j].keys[:nb], "\x00")
		if bi != bj {
			return bi < bj
		}
		ci, cj := groups[i].vals[0].count, groups[j].vals[0].count
		if ci != cj {
			return (ci > cj) != c.rare
		}
		return strings.Join(groups[i].keys, "\x00") < strings.Join(groups[j].keys, "\x00")
	})
	cols := append(append([]string(nil), c.by...), "count", "percent")
	var rows [][]string
	seen := map[string]int{}
	for _, g := range groups {
		b := strings.Join(g.keys[:nb], "\x00")
		if seen[b] >= c.limit {
			continue
		}
		seen[b]++
		pct := 100 * float64(g.vals[0].count) / float64(max(1, totals[b]))
		row := append(append([]string(nil), g.keys...), strconv.FormatInt(g.vals[0].count, 10),
			strconv.FormatFloat(pct, 'f', 2, 64))
		rows = append(rows, row)
	}
	makeTable(cols, rows, emit)
}

func (s *statsStage) Snapshot(emit Emit) { s.emitRows(emit) }

func (s *statsStage) Finish(emit Emit) {
	if s.partial {
		s.flushPartial(emit)
		return
	}
	s.emitRows(emit)
}

func statsSpec(name string, c *statsCfg) *Spec {
	return &Spec{
		Name:       name,
		Blocking:   true,
		New:        func() Stage { return &statsStage{c: c} },
		NewPartial: func() Stage { return &statsStage{c: c, partial: true, lastFlush: time.Now()} },
	}
}

var aggFuncs = map[string]string{
	"count": "count", "c": "count", "dc": "dc", "distinct_count": "dc", "sum": "sum",
	"avg": "avg", "mean": "avg", "min": "min", "max": "max", "median": "perc",
	"values": "values", "first": "first", "last": "last",
}

var rePerc = regexp.MustCompile(`^(?:p|perc)(\d{1,2}(?:\.\d+)?)$`)

// parseStats parses "agg[(field)] [as name], ... [by f1, f2]".
func parseStats(args []word) (*Spec, error) {
	src, pos := rawArg(args)
	toks, err := lexExpr(src, pos)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks}
	c := &statsCfg{}
	if c.aggs, err = parseAggs(p, "stats"); err != nil {
		return nil, err
	}
	if p.isKw("by") {
		p.next()
		if c.by, err = parseFieldList(p, "stats"); err != nil {
			return nil, err
		}
	}
	if p.peek().kind != tEOF {
		return nil, p.errExpected("',', by or end of stats")
	}
	return statsSpec("stats", c), nil
}

// parseAggs parses "agg[(field)] [as name], ...".
func parseAggs(p *exprParser, cmd string) ([]aggSpec, error) {
	var aggs []aggSpec
	for {
		t := p.next()
		if t.kind != tIdent {
			return nil, errAt(t.pos, "%s: aggregation expected, e.g. %s count, avg(ms) by host", cmd, cmd)
		}
		low := strings.ToLower(t.s)
		a := aggSpec{fn: aggFuncs[low]}
		if m := rePerc.FindStringSubmatch(low); m != nil {
			a.fn = "perc"
			a.pct, _ = strconv.ParseFloat(m[1], 64)
		} else if low == "median" {
			a.pct = 50
		}
		if a.fn == "" {
			return nil, errAt(t.pos, "%s: unknown aggregation %q (count dc sum avg min max median pNN values first last)", cmd, t.s)
		}
		if p.isOp("(") {
			p.next()
			f := p.next()
			if f.kind != tIdent && f.kind != tField {
				return nil, errAt(f.pos, "%s: field name expected", cmd)
			}
			a.field = f.s
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
		} else if a.fn != "count" {
			return nil, p.errExpected("'(' field ')' after " + t.s)
		}
		a.name = low
		if a.field != "" {
			a.name = low + "(" + a.field + ")"
		}
		if p.isKw("as") {
			p.next()
			n := p.next()
			if n.kind != tIdent && n.kind != tField {
				return nil, errAt(n.pos, "%s: name expected after as", cmd)
			}
			a.name = n.s
		}
		aggs = append(aggs, a)
		if p.isOp(",") {
			p.next()
			continue
		}
		return aggs, nil
	}
}

func parseFieldList(p *exprParser, cmd string) ([]string, error) {
	var out []string
	for {
		t := p.peek()
		if t.kind != tIdent && t.kind != tField {
			if len(out) > 0 && t.kind == tEOF {
				return out, nil
			}
			if t.kind == tEOF {
				return nil, errAt(p.eofPos(), "%s: field name expected", cmd)
			}
			return nil, errAt(t.pos, "%s: field name expected", cmd)
		}
		if t.kind == tIdent && strings.EqualFold(t.s, "by") {
			return out, nil
		}
		p.next()
		out = append(out, t.s)
		if p.isOp(",") {
			p.next()
		}
		if p.peek().kind == tEOF {
			return out, nil
		}
	}
}

// parseTop parses "top [N] [limit=N] field... [by field...]" (also rare).
func parseTop(name string, args []word) (*Spec, error) {
	src, pos := rawArg(args)
	toks, err := lexExpr(src, pos)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks}
	c := &statsCfg{top: name == "top", rare: name == "rare", limit: 10,
		aggs: []aggSpec{{fn: "count", name: "count"}}}
	if t := p.peek(); t.kind == tNum {
		p.next()
		c.limit, _ = strconv.Atoi(t.s)
	}
	if p.isKw("limit") {
		p.next()
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		t := p.next()
		n, err := strconv.Atoi(t.s)
		if t.kind != tNum || err != nil {
			return nil, errAt(t.pos, "%s: number expected after limit=", name)
		}
		c.limit = n
	}
	if c.topField, err = parseFieldList(p, name); err != nil {
		return nil, err
	}
	var by []string
	if p.isKw("by") {
		p.next()
		if by, err = parseFieldList(p, name); err != nil {
			return nil, err
		}
	}
	if p.peek().kind != tEOF {
		return nil, p.errExpected("by or end of " + name)
	}
	c.by = append(by, c.topField...)
	return statsSpec(name, c), nil
}

// ---------------- table ----------------

type tableStage struct {
	cols []string
	rows [][]string
}

func (s *tableStage) Push(l Line, emit Emit) bool {
	row := make([]string, len(s.cols))
	for i, c := range s.cols {
		row[i], _ = l.Field(c)
	}
	s.rows = append(s.rows, row)
	return true
}
func (s *tableStage) Snapshot(emit Emit) { makeTable(s.cols, s.rows, emit) }
func (s *tableStage) Finish(emit Emit)   { makeTable(s.cols, s.rows, emit) }

func parseTable(args []word) (*Spec, error) {
	src, pos := rawArg(args)
	toks, err := lexExpr(src, pos)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks}
	cols, err := parseFieldList(p, "table")
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.errExpected("field name")
	}
	return &Spec{Name: "table", Blocking: true, New: func() Stage { return &tableStage{cols: cols} }}, nil
}

// ---------------- fieldsummary ----------------

type fieldStat struct {
	count    int64
	distinct map[string]int64
	numeric  int64
	min, max float64
}

type fieldSummaryStage struct {
	events int64
	fields map[string]*fieldStat
}

const maxDistinctTracked = 1000

func (s *fieldSummaryStage) Push(l Line, emit Emit) bool {
	if s.fields == nil {
		s.fields = map[string]*fieldStat{}
	}
	s.events++
	for _, name := range l.FieldNames() {
		v, _ := l.Field(name)
		fs := s.fields[name]
		if fs == nil {
			fs = &fieldStat{distinct: map[string]int64{}}
			s.fields[name] = fs
		}
		fs.count++
		if _, ok := fs.distinct[v]; ok || len(fs.distinct) < maxDistinctTracked {
			fs.distinct[v]++
		}
		if f, ok := parseNum(v); ok {
			if fs.numeric == 0 || f < fs.min {
				fs.min = f
			}
			if fs.numeric == 0 || f > fs.max {
				fs.max = f
			}
			fs.numeric++
		}
	}
	return true
}

func (s *fieldSummaryStage) emitAll(emit Emit) {
	names := make([]string, 0, len(s.fields))
	for n := range s.fields {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := s.fields[names[i]], s.fields[names[j]]
		if a.count != b.count {
			return a.count > b.count
		}
		return names[i] < names[j]
	})
	var rows [][]string
	for _, n := range names {
		fs := s.fields[n]
		dc := strconv.Itoa(len(fs.distinct))
		if len(fs.distinct) >= maxDistinctTracked {
			dc += "+"
		}
		mn, mx := "", ""
		if fs.numeric == fs.count {
			mn, mx = formatNum(fs.min), formatNum(fs.max)
		}
		rows = append(rows, []string{n, strconv.FormatInt(fs.count, 10),
			strconv.FormatFloat(100*float64(fs.count)/float64(max(1, s.events)), 'f', 1, 64),
			dc, mn, mx, topValues(fs.distinct, 3)})
	}
	makeTable([]string{"field", "count", "coverage%", "distinct", "min", "max", "top values"}, rows, emit)
}

func topValues(m map[string]int64, n int) string {
	type kv struct {
		v string
		c int64
	}
	all := make([]kv, 0, len(m))
	for v, c := range m {
		all = append(all, kv{v, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].c != all[j].c {
			return all[i].c > all[j].c
		}
		return all[i].v < all[j].v
	})
	var parts []string
	for i := 0; i < len(all) && i < n; i++ {
		v := all[i].v
		if len([]rune(v)) > 20 {
			v = string([]rune(v)[:19]) + "…"
		}
		parts = append(parts, fmt.Sprintf("%s(%d)", v, all[i].c))
	}
	return strings.Join(parts, " ")
}

func (s *fieldSummaryStage) Snapshot(emit Emit) { s.emitAll(emit) }
func (s *fieldSummaryStage) Finish(emit Emit)   { s.emitAll(emit) }

func parseFieldSummary(args []word) (*Spec, error) {
	if src, pos := rawArg(args); strings.TrimSpace(src) != "" {
		return nil, errAt(pos, "fieldsummary: takes no arguments")
	}
	return &Spec{Name: "fieldsummary", Blocking: true, New: func() Stage { return &fieldSummaryStage{} }}, nil
}

// ---------------- sort by field ----------------

type sortTerm struct {
	field string
	desc  bool
}

type fieldSortStage struct {
	terms []sortTerm
	limit int
	lines []sortedLine // with a limit: a heap whose root is the line that sorts last
	seq   int
}

type sortedLine struct {
	l   Line
	seq int // arrival order, so equal keys keep their order
}

// less reports whether a sorts before b.
func (s *fieldSortStage) less(a, b *sortedLine) bool {
	for _, t := range s.terms {
		x, okx := a.l.Field(t.field)
		y, oky := b.l.Field(t.field)
		if okx != oky {
			return okx // missing values last
		}
		if r := compareValues(x, y); r != 0 {
			return (r < 0) != t.desc
		}
	}
	return a.seq < b.seq
}

func (s *fieldSortStage) Push(l Line, emit Emit) bool {
	for _, t := range s.terms {
		l.Field(t.field) // extract fields now, while the line is owned by this stage
	}
	it := sortedLine{l: l, seq: s.seq}
	s.seq++
	if s.limit <= 0 {
		s.lines = append(s.lines, it)
		return true
	}
	// sort N: keep only the first N lines in a heap, so memory stays bounded
	h := s.lines
	if len(h) < s.limit {
		h = append(h, it)
		for i := len(h) - 1; i > 0; {
			p := (i - 1) / 2
			if !s.less(&h[p], &h[i]) {
				break
			}
			h[p], h[i] = h[i], h[p]
			i = p
		}
		s.lines = h
		return true
	}
	if !s.less(&it, &h[0]) {
		return true
	}
	h[0] = it
	for i := 0; ; {
		c := 2*i + 1
		if c >= len(h) {
			break
		}
		if c+1 < len(h) && s.less(&h[c], &h[c+1]) {
			c++
		}
		if !s.less(&h[i], &h[c]) {
			break
		}
		h[i], h[c] = h[c], h[i]
		i = c
	}
	return true
}

func (s *fieldSortStage) emitAll(emit Emit) {
	out := s.lines
	if s.limit > 0 {
		out = append([]sortedLine(nil), s.lines...) // keep the heap intact for later snapshots
	}
	sort.Slice(out, func(i, j int) bool { return s.less(&out[i], &out[j]) })
	for i := range out {
		if s.limit > 0 && i >= s.limit {
			return
		}
		if !emit(out[i].l) {
			return
		}
	}
}

func (s *fieldSortStage) Snapshot(emit Emit) { s.emitAll(emit) }
func (s *fieldSortStage) Finish(emit Emit)   { s.emitAll(emit) }

var reFieldSortArg = regexp.MustCompile(`^[-+]?[A-Za-z_][\w.]*$`)

// fieldSortArgs reports whether sort's arguments are field terms ("sort -count",
// "sort 10 -count host") rather than POSIX options.
func fieldSortArgs(args []word) bool {
	if len(args) == 0 {
		return false
	}
	for i, a := range args {
		s := a.s
		if i == 0 && isAllDigits(s) {
			continue
		}
		if s == "-" || s == "+" {
			continue
		}
		if !reFieldSortArg.MatchString(s) {
			return false
		}
		if strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "--") {
			// "-rn" is a POSIX flag group; "-count" is a field.
			if strings.Trim(s[1:], "rnufbsdg") == "" || s[1] == 'k' || s[1] == 't' {
				return false
			}
		}
	}
	return true
}

func isAllDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsDigit(r) }) < 0
}

func parseFieldSort(args []word) *Spec {
	s := &fieldSortStage{}
	desc := false
	for i, a := range args {
		v := a.s
		switch {
		case i == 0 && isAllDigits(v):
			s.limit, _ = strconv.Atoi(v)
		case v == "-":
			desc = true
		case v == "+":
			desc = false
		default:
			t := sortTerm{field: strings.TrimLeft(v, "+-"), desc: desc || strings.HasPrefix(v, "-")}
			s.terms = append(s.terms, t)
			desc = false
		}
	}
	terms, limit := s.terms, s.limit
	return &Spec{Name: "sort", Blocking: true, New: func() Stage { return &fieldSortStage{terms: terms, limit: limit} }}
}

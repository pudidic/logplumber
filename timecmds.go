package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Time commands: search earliest=/latest=, bin (bucket) and timechart.

// ---------------- search ----------------

type timeRange struct {
	earliest, latest int64 // Unix milliseconds; earliest inclusive, latest exclusive
	hasE, hasL       bool
}

func (r timeRange) contains(ms int64) bool {
	return (!r.hasE || ms >= r.earliest) && (!r.hasL || ms < r.latest)
}

type searchStage struct{ r timeRange }

func (s searchStage) Push(l Line, emit Emit) bool {
	ms, ok := l.lineTime()
	if ok && s.r.contains(ms) {
		return emit(l)
	}
	return true
}
func (searchStage) Finish(Emit) {}

// parseSearch parses "search earliest=<time> latest=<time>". Events without a
// timestamp are dropped when a time range is given.
func parseSearch(args []word) (*Spec, error) {
	now := time.Now()
	var r timeRange
	for _, a := range args {
		k, v, ok := strings.Cut(a.s, "=")
		k = strings.ToLower(k)
		if !ok || a.quoted || k != "earliest" && k != "latest" {
			return nil, errAt(a.pos, "search: %q not supported yet (use earliest=-24h latest=now)", a.s)
		}
		t, err := parseTimeSpec(v, now)
		if err != nil {
			return nil, errAt(a.pos, "search: %v", err)
		}
		if k == "earliest" {
			r.earliest, r.hasE = t.UnixMilli(), true
		} else {
			r.latest, r.hasL = t.UnixMilli(), true
		}
	}
	if len(args) == 0 {
		return &Spec{Name: "search", Parallel: true, New: func() Stage { return passStage{} }}, nil
	}
	return &Spec{Name: "search", Parallel: true, New: func() Stage { return searchStage{r: r} }}, nil
}

// ---------------- bin ----------------

type binStage struct {
	field, as string
	sp        span    // for _time
	step      float64 // for numeric fields
}

func (s binStage) Push(l Line, emit Emit) bool {
	if s.field == "_time" {
		if ms, ok := l.lineTime(); ok {
			l.SetField(s.as, strconv.FormatInt(s.sp.floor(time.UnixMilli(ms)).Unix(), 10))
		}
		return emit(l)
	}
	v, ok := l.Field(s.field)
	if !ok {
		return emit(l)
	}
	f, ok := parseNum(v)
	if !ok {
		return emit(l)
	}
	lo := math.Floor(f/s.step) * s.step
	l.SetField(s.as, formatNum(lo)+"-"+formatNum(lo+s.step))
	return emit(l)
}
func (binStage) Finish(Emit) {}

// parseBin parses "bin [field] span=<span> [as name]"; the field defaults to _time.
func parseBin(name string, args []word) (*Spec, error) {
	s := binStage{field: "_time"}
	spanText, spanPos := "", 0
	fieldSet := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(strings.ToLower(a.s), "span="):
			spanText, spanPos = a.s[5:], a.pos
		case strings.EqualFold(a.s, "as"):
			if i+1 >= len(args) {
				return nil, errAt(a.pos, "%s: field name expected after as", name)
			}
			i++
			s.as = args[i].s
		case !fieldSet && !strings.Contains(a.s, "="):
			s.field, fieldSet = a.s, true
		default:
			return nil, errAt(a.pos, "%s: unexpected %q (usage: %s [field] span=5m [as name])", name, a.s, name)
		}
	}
	end := 0
	if len(args) > 0 {
		end = args[len(args)-1].pos
	}
	if spanText == "" {
		return nil, errAt(end, "%s: span= expected, e.g. %s span=5m", name, name)
	}
	if s.field == "_time" {
		sp, err := parseSpan(spanText)
		if err != nil {
			return nil, errAt(spanPos, "%s: %v", name, err)
		}
		s.sp = sp
	} else {
		f, ok := parseNum(spanText)
		if !ok || f <= 0 {
			return nil, errAt(spanPos, "%s: numeric span expected for field %s", name, s.field)
		}
		s.step = f
	}
	if s.as == "" {
		s.as = s.field
	}
	return &Spec{Name: name, Parallel: true, New: func() Stage { return s }}, nil
}

// ---------------- timechart ----------------

type timechartCfg struct {
	span  *span // nil: picked automatically
	limit int   // series kept when splitting by a field (0 = all); the rest is OTHER
}

// maxAutoBuckets is how many time buckets an automatic span may produce before
// timechart switches to the next coarser span.
const maxAutoBuckets = 100

func (st *statsState) span(c *statsCfg) span {
	if c.tc.span != nil {
		return *c.tc.span
	}
	return autoSpans[st.auto]
}

// newGroup records the time bucket of a new group; it reports whether the
// automatic span now has too many buckets.
func (st *statsState) newGroup(c *statsCfg, g *group) bool {
	if c.tc == nil || c.tc.span != nil {
		return false
	}
	if st.buckets == nil {
		st.buckets = map[string]struct{}{}
	}
	st.buckets[g.keys[0]] = struct{}{}
	return len(st.buckets) > maxAutoBuckets
}

func (st *statsState) checkBuckets(c *statsCfg) {
	for c.tc != nil && c.tc.span == nil && len(st.buckets) > maxAutoBuckets && st.auto < len(autoSpans)-1 {
		st.coarsen(c, st.auto+1)
	}
}

// coarsen rebuckets the groups to autoSpans[to]. Each automatic span divides the
// next, so a bucket always falls entirely into one coarser bucket.
func (st *statsState) coarsen(c *statsCfg, to int) {
	sp := autoSpans[to]
	old := st.groups
	st.groups, st.buckets, st.auto = make(map[string]*group, len(old)), map[string]struct{}{}, to
	for _, g := range old {
		sec, _ := strconv.ParseInt(g.keys[0], 10, 64)
		g.keys[0] = strconv.FormatInt(sp.floor(time.Unix(sec, 0)).Unix(), 10)
		k := strings.Join(g.keys, "\x00")
		if e := st.groups[k]; e != nil {
			for i := range c.aggs {
				mergeAgg(c.aggs[i].fn, &e.vals[i], &g.vals[i])
			}
			continue
		}
		st.groups[k] = g
		st.buckets[g.keys[0]] = struct{}{}
	}
}

const (
	barWidth     = 40
	maxFillSteps = 100000 // empty buckets are filled in only up to this many rows
)

var barShades = []string{"█", "▓", "▒", "░"}

func (s *statsStage) emitTimechart(groups []*group, emit Emit) {
	c := s.c
	sp := s.state().span(c)
	split := len(c.by) > 1

	// series: by-field values, limited to the largest ones by the first aggregation
	var values []string
	other := false
	if split {
		totals := map[string]float64{}
		for _, g := range groups {
			f, _ := parseNum(g.vals[0].result(c.aggs[0]))
			totals[g.keys[1]] += f
		}
		for v := range totals {
			values = append(values, v)
		}
		if c.tc.limit > 0 && len(values) > c.tc.limit {
			sort.Slice(values, func(i, j int) bool {
				if totals[values[i]] != totals[values[j]] {
					return totals[values[i]] > totals[values[j]]
				}
				return lessValue(values[i], values[j])
			})
			values, other = values[:c.tc.limit], true
		}
		sort.Slice(values, func(i, j int) bool { return lessValue(values[i], values[j]) })
	}
	keep := map[string]int{}
	for i, v := range values {
		keep[v] = i
	}
	nv := len(values)
	if other {
		nv++
	}

	// cells[bucket][series value][agg]
	type cell = []*aggVal
	cells := map[int64][]cell{}
	for _, g := range groups {
		sec, _ := strconv.ParseInt(g.keys[0], 10, 64)
		row := cells[sec]
		if row == nil {
			row = make([]cell, max(1, nv))
			cells[sec] = row
		}
		vi := 0
		if split {
			i, ok := keep[g.keys[1]]
			if !ok {
				i = nv - 1 // OTHER
			}
			vi = i
		}
		if row[vi] == nil {
			row[vi] = make(cell, len(c.aggs))
		}
		for j := range c.aggs {
			if !other || vi < nv-1 {
				row[vi][j] = &g.vals[j] // one group per bucket and series
				continue
			}
			if row[vi][j] == nil {
				row[vi][j] = &aggVal{}
			}
			mergeAgg(c.aggs[j].fn, row[vi][j], &g.vals[j])
		}
	}

	// rows in time order, with empty buckets filled in
	var times []int64
	for t := range cells {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	if len(times) > 1 {
		var filled []int64
		end := time.Unix(times[len(times)-1], 0)
		for t := time.Unix(times[0], 0); !t.After(end); t = sp.next(t) {
			if filled = append(filled, t.Unix()); len(filled) > maxFillSteps {
				filled = nil
				break
			}
		}
		if filled != nil {
			times = filled
		}
	}

	// columns
	cols := []string{"_time"}
	names := append([]string(nil), values...)
	if other {
		names = append(names, "OTHER")
	}
	for _, a := range c.aggs {
		if !split {
			cols = append(cols, a.name)
			continue
		}
		for _, v := range names {
			if len(c.aggs) > 1 {
				v = a.name + ": " + v
			}
			cols = append(cols, v)
		}
	}
	nseries := len(cols) - 1

	rows := make([][]string, len(times))
	nums := make([][]float64, len(times))
	for i, t := range times {
		row := []string{strconv.FormatInt(t, 10)}
		vals := cells[t]
		for j, a := range c.aggs {
			for vi := 0; vi < max(1, nv); vi++ {
				var r string
				if vals != nil && vals[vi] != nil && vals[vi][j] != nil {
					r = vals[vi][j].result(a)
				} else if a.fn == "count" || a.fn == "dc" {
					r = "0"
				}
				row = append(row, r)
			}
		}
		rows[i] = row
		nums[i] = make([]float64, nseries)
		for k := range nseries {
			nums[i][k], _ = parseNum(row[k+1])
		}
	}

	// bar chart column
	stack := nseries > 1
	for _, a := range c.aggs {
		if a.fn != "count" && a.fn != "sum" {
			stack = false
		}
	}
	if nseries == 1 || stack {
		cols = append(cols, barLegend(cols[1:], stack))
		peak := 0.0
		neg := false
		for _, r := range nums {
			tot := 0.0
			for _, v := range r {
				neg = neg || v < 0
				tot += v
			}
			peak = math.Max(peak, tot)
		}
		if !neg {
			for i := range rows {
				rows[i] = append(rows[i], barText(nums[i], peak, stack && nseries <= len(barShades)))
			}
		} else {
			cols = cols[:len(cols)-1]
		}
	}
	makeTable(cols, rows, emit)
}

// barLegend is the header of the bar column.
func barLegend(series []string, stack bool) string {
	if !stack || len(series) > len(barShades) {
		if len(series) == 1 {
			return "█ " + series[0]
		}
		return "█ total"
	}
	parts := make([]string, len(series))
	for i, s := range series {
		parts[i] = barShades[i] + " " + s
	}
	return strings.Join(parts, "  ")
}

var eighths = []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// barText draws one row: stacked shades per series, or a single bar of the total
// with eighth-block precision.
func barText(vals []float64, peak float64, shaded bool) string {
	if peak <= 0 {
		return ""
	}
	var b strings.Builder
	if !shaded {
		tot := 0.0
		for _, v := range vals {
			tot += v
		}
		n := int(math.Round(tot / peak * barWidth * 8))
		b.WriteString(strings.Repeat("█", n/8))
		b.WriteString(eighths[n%8])
		return b.String()
	}
	cum, prev := 0.0, 0
	for i, v := range vals {
		cum += v
		end := int(math.Round(cum / peak * barWidth))
		b.WriteString(strings.Repeat(barShades[i], end-prev))
		prev = end
	}
	return b.String()
}

// spanArg reads a span value that the expression lexer split into a number and
// a unit ("5m" -> 5, m).
func spanArg(p *exprParser) (string, int, error) {
	t := p.next()
	switch t.kind {
	case tNum:
		if u := p.peek(); u.kind == tIdent && u.pos == t.pos+len([]rune(t.s)) {
			p.next()
			return t.s + u.s, t.pos, nil
		}
		return t.s, t.pos, nil
	case tIdent, tStr:
		return t.s, t.pos, nil
	}
	return "", t.pos, errAt(t.pos, "timechart: span expected, e.g. span=5m")
}

// timechartOptions parses "span=X" and "limit=N" options.
func timechartOptions(p *exprParser, tc *timechartCfg) error {
	for p.isKw("span") || p.isKw("limit") {
		kw := strings.ToLower(p.next().s)
		if err := p.expectOp("="); err != nil {
			return err
		}
		if kw == "limit" {
			t := p.next()
			n, err := strconv.Atoi(t.s)
			if t.kind != tNum || err != nil || n < 0 {
				return errAt(t.pos, "timechart: number expected after limit=")
			}
			tc.limit = n
			continue
		}
		v, pos, err := spanArg(p)
		if err != nil {
			return err
		}
		sp, err := parseSpan(v)
		if err != nil {
			return errAt(pos, "timechart: %v", err)
		}
		tc.span = &sp
	}
	return nil
}

// parseTimechart parses "timechart [span=X] [limit=N] agg[(field)] [as name], ... [by field]".
func parseTimechart(args []word) (*Spec, error) {
	src, pos := rawArg(args)
	toks, err := lexExpr(src, pos)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks}
	tc := &timechartCfg{limit: 10}
	c := &statsCfg{tc: tc, by: []string{"_time"}}
	if err := timechartOptions(p, tc); err != nil {
		return nil, err
	}
	if c.aggs, err = parseAggs(p, "timechart"); err != nil {
		return nil, err
	}
	if p.isKw("by") {
		p.next()
		t := p.next()
		if t.kind != tIdent && t.kind != tField {
			return nil, errAt(t.pos, "timechart: field name expected after by")
		}
		c.by = append(c.by, t.s)
	}
	if err := timechartOptions(p, tc); err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.errExpected("by, span=, limit= or end of timechart")
	}
	return statsSpec("timechart", c), nil
}

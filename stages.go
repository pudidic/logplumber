package main

import (
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Line is one line of text flowing through a pipeline.
type Line struct {
	Text string
	Src  string      // display name of the file the line came from
	No   int         // 1-based line number within Src
	Raw  int         // original byte length including newline (0 = unknown)
	Rec  *Record     // extracted or computed fields (nil until first used)
	Agg  *statsState // partial aggregation sent from a worker thread
}

// Emit passes a line downstream. It returns false when downstream wants no more input.
type Emit func(Line) bool

// Stage is one command of a pipeline.
type Stage interface {
	Push(l Line, emit Emit) bool
	Finish(emit Emit)
}

// Snapshotter is implemented by blocking stages (sort, wc, tail) so the shell can
// render their current result while input is still being read.
type Snapshotter interface {
	Snapshot(emit Emit)
}

// FileEnder is notified when a worker finishes one input file (grep -c/-l per file).
type FileEnder interface {
	EndFile(src string, emit Emit) bool
}

type planCtx struct {
	first bool // stage is the first command and sees per-file context
	multi bool // input consists of more than one file
}

// Spec describes a parsed command; New creates an independent instance.
type Spec struct {
	Name     string
	Parallel bool // stateless per line: may run inside worker threads
	Blocking bool // emits only after all input (has Snapshot)
	New      func() Stage
	// NewPartial, when set, creates a worker-side instance that pre-aggregates and
	// sends partial results to the New instance (stats, top).
	NewPartial func() Stage
	Setup      func(pc planCtx, s *Spec)
	limit      int        // head N (SPL): lets an implicit time sort keep only N lines
	out        string     // file written by the command (outputcsv)
	rt         *followCfg // real-time search (SPL earliest=rt...) or tail -f
	need       [][]string // first command: text a line must contain (see indexNeeds)
}

// ---------------- grep ----------------

type grepCfg struct {
	re                    *regexp.Regexp
	invert, count, number bool
	only, list, listNot   bool
	forceName, noName     bool
	withName, perFile     bool
}

type grepStage struct {
	c       *grepCfg
	n, cnt  int
	matched bool
}

func (g *grepStage) out(l Line, lineNo int, text string) Line {
	c := g.c
	if !c.withName && !c.number && text == l.Text {
		return l // unchanged line keeps its fields
	}
	if c.withName || c.number {
		var b strings.Builder
		if c.withName {
			b.WriteString(l.Src)
			b.WriteByte(':')
		}
		if c.number {
			b.WriteString(strconv.Itoa(lineNo))
			b.WriteByte(':')
		}
		b.WriteString(text)
		text = b.String()
	}
	return Line{Text: text, Src: l.Src, No: l.No}
}

func (g *grepStage) Push(l Line, emit Emit) bool {
	c := g.c
	g.n++
	lineNo := g.n
	if c.perFile {
		lineNo = l.No
	}
	if c.only && !c.invert {
		ms := c.re.FindAllString(l.Text, -1)
		if len(ms) == 0 {
			return true
		}
		g.cnt++
		g.matched = true
		if c.count || c.list || c.listNot {
			return true
		}
		for _, m := range ms {
			if m == "" {
				continue
			}
			if !emit(g.out(l, lineNo, m)) {
				return false
			}
		}
		return true
	}
	if c.re.MatchString(l.Text) == c.invert {
		return true
	}
	g.cnt++
	g.matched = true
	if c.count || c.list || c.listNot {
		return true
	}
	return emit(g.out(l, lineNo, l.Text))
}

func (g *grepStage) EndFile(src string, emit Emit) bool {
	c := g.c
	if !c.perFile {
		return true
	}
	ok := true
	switch {
	case c.count:
		if c.withName {
			ok = emit(Line{Text: src + ":" + strconv.Itoa(g.cnt), Src: src})
		} else {
			ok = emit(Line{Text: strconv.Itoa(g.cnt), Src: src})
		}
	case c.list && g.matched, c.listNot && !g.matched:
		ok = emit(Line{Text: src, Src: src})
	}
	g.cnt, g.matched = 0, false
	return ok
}

func (g *grepStage) Finish(emit Emit) {
	c := g.c
	if c.perFile {
		return
	}
	switch {
	case c.count:
		emit(Line{Text: strconv.Itoa(g.cnt)})
	case c.list && g.matched, c.listNot && !g.matched:
		emit(Line{Text: "(standard input)"})
	}
}

// breToEre converts a POSIX basic regular expression to the RE2 (extended) syntax.
func breToEre(s string) string {
	var b strings.Builder
	rs := []rune(s)
	inBracket := false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if inBracket {
			b.WriteRune(c)
			if c == ']' && !(i > 0 && (rs[i-1] == '[' || (rs[i-1] == '^' && i > 1 && rs[i-2] == '['))) {
				inBracket = false
			}
			continue
		}
		switch {
		case c == '[':
			inBracket = true
			b.WriteRune(c)
		case c == '\\' && i+1 < len(rs):
			n := rs[i+1]
			if strings.ContainsRune("(){}|+?", n) {
				b.WriteRune(n)
			} else {
				b.WriteRune('\\')
				b.WriteRune(n)
			}
			i++
		case strings.ContainsRune("(){}|+?", c):
			b.WriteRune('\\')
			b.WriteRune(c)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func compilePattern(pats []string, fixed, extended, word, whole, icase bool) (*regexp.Regexp, error) {
	parts := make([]string, len(pats))
	for i, p := range pats {
		switch {
		case fixed:
			p = regexp.QuoteMeta(p)
		case !extended:
			p = breToEre(p)
		}
		parts[i] = "(?:" + p + ")"
	}
	expr := strings.Join(parts, "|")
	if word {
		expr = `\b(?:` + expr + `)\b`
	}
	if whole {
		expr = `^(?:` + expr + `)$`
	}
	if icase {
		expr = "(?i)" + expr
	}
	return regexp.Compile(expr)
}

// ---------------- sort ----------------

type sortCfg struct {
	reverse, numeric, unique, fold, stable bool
	k1, k2                                 int
	sep                                    string
}

type sortItem struct {
	l   Line
	key string
	num float64
}

type sortStage struct {
	c     *sortCfg
	items []sortItem
}

func fieldsOf(s, sep string) []string {
	if sep == "" {
		return strings.Fields(s)
	}
	return strings.Split(s, sep)
}

func (c *sortCfg) keyOf(text string) string {
	key := text
	if c.k1 > 0 {
		f := fieldsOf(text, c.sep)
		if c.k1 > len(f) {
			key = ""
		} else {
			end := len(f)
			if c.k2 > 0 && c.k2 < end {
				end = c.k2
			}
			if end < c.k1 {
				end = c.k1
			}
			j := c.sep
			if j == "" {
				j = " "
			}
			key = strings.Join(f[c.k1-1:end], j)
		}
	}
	if c.fold {
		key = strings.ToLower(key)
	}
	return key
}

func leadingNumber(s string) float64 {
	s = strings.TrimLeft(s, " \t")
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	dot := false
	for end < len(s) {
		ch := s[end]
		if ch >= '0' && ch <= '9' {
			end++
		} else if ch == '.' && !dot {
			dot = true
			end++
		} else {
			break
		}
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0
	}
	return v
}

func (s *sortStage) Push(l Line, emit Emit) bool {
	it := sortItem{l: l, key: s.c.keyOf(l.Text)}
	if s.c.numeric {
		it.num = leadingNumber(it.key)
	}
	s.items = append(s.items, it)
	return true
}

func (s *sortStage) cmpKey(a, b *sortItem) int {
	if s.c.numeric {
		switch {
		case a.num < b.num:
			return -1
		case a.num > b.num:
			return 1
		}
		return 0
	}
	return strings.Compare(a.key, b.key)
}

func (s *sortStage) cmp(a, b sortItem) int {
	r := s.cmpKey(&a, &b)
	if r == 0 && !s.c.unique && !s.c.stable {
		r = strings.Compare(a.l.Text, b.l.Text)
	}
	if s.c.reverse {
		r = -r
	}
	return r
}

func (s *sortStage) emitAll(emit Emit) {
	s.items = parallelSort(s.items, s.cmp)
	for i := range s.items {
		if s.c.unique && i > 0 && s.cmpKey(&s.items[i-1], &s.items[i]) == 0 {
			continue
		}
		if !emit(s.items[i].l) {
			return
		}
	}
}

func (s *sortStage) Snapshot(emit Emit) { s.emitAll(emit) }
func (s *sortStage) Finish(emit Emit)   { s.emitAll(emit) }

// parallelSort sorts chunks on all CPUs and merges them pairwise in parallel.
func parallelSort[T any](items []T, cmp func(a, b T) int) []T {
	n := len(items)
	p := runtime.NumCPU()
	if n < 50000 || p < 2 {
		slices.SortStableFunc(items, cmp)
		return items
	}
	chunk := (n + p - 1) / p
	var wg sync.WaitGroup
	for i := 0; i < n; i += chunk {
		j := min(i+chunk, n)
		wg.Add(1)
		go func(s []T) { defer wg.Done(); slices.SortStableFunc(s, cmp) }(items[i:j])
	}
	wg.Wait()
	src, dst := items, make([]T, n)
	for width := chunk; width < n; width *= 2 {
		for i := 0; i < n; i += 2 * width {
			mid, hi := min(i+width, n), min(i+2*width, n)
			wg.Add(1)
			go func(lo, mid, hi int) {
				defer wg.Done()
				a, b, out := src[lo:mid], src[mid:hi], dst[lo:hi]
				x, y, k := 0, 0, 0
				for x < len(a) && y < len(b) {
					if cmp(b[y], a[x]) < 0 {
						out[k] = b[y]
						y++
					} else {
						out[k] = a[x]
						x++
					}
					k++
				}
				k += copy(out[k:], a[x:])
				copy(out[k:], b[y:])
			}(i, mid, hi)
		}
		wg.Wait()
		src, dst = dst, src
	}
	return src
}

// ---------------- uniq ----------------

type uniqCfg struct{ count, dups, uniques, fold bool }

type uniqStage struct {
	c    *uniqCfg
	prev Line
	key  string
	n    int
}

func (u *uniqStage) flush(emit Emit) bool {
	if u.n == 0 {
		return true
	}
	if (u.c.dups && u.n < 2) || (u.c.uniques && u.n > 1) {
		return true
	}
	t := u.prev.Text
	if u.c.count {
		t = fmt.Sprintf("%7d %s", u.n, t)
	}
	if !u.c.count {
		return emit(u.prev)
	}
	return emit(Line{Text: t, Src: u.prev.Src})
}

func (u *uniqStage) Push(l Line, emit Emit) bool {
	k := l.Text
	if u.c.fold {
		k = strings.ToLower(k)
	}
	if u.n > 0 && k == u.key {
		u.n++
		return true
	}
	ok := u.flush(emit)
	u.prev, u.key, u.n = l, k, 1
	return ok
}

func (u *uniqStage) Finish(emit Emit) { u.flush(emit) }

// ---------------- wc ----------------

type wcCfg struct {
	lines, words, bytes, chars bool
	perFile                    bool
}

type wcCounts struct{ l, w, c, m int64 }

type wcStage struct {
	c     *wcCfg
	total wcCounts
	per   map[string]*wcCounts
}

func (w *wcStage) Push(l Line, emit Emit) bool {
	raw := int64(l.Raw)
	if raw == 0 {
		raw = int64(len(l.Text)) + 1
	}
	d := wcCounts{l: 1, c: raw}
	if w.c.words {
		d.w = int64(len(strings.Fields(l.Text)))
	}
	if w.c.chars {
		d.m = int64(utf8.RuneCountInString(l.Text)) + raw - int64(len(l.Text))
	}
	add := func(t *wcCounts) { t.l += d.l; t.w += d.w; t.c += d.c; t.m += d.m }
	add(&w.total)
	if w.c.perFile {
		if w.per == nil {
			w.per = map[string]*wcCounts{}
		}
		p := w.per[l.Src]
		if p == nil {
			p = &wcCounts{}
			w.per[l.Src] = p
		}
		add(p)
	}
	return true
}

func (w *wcStage) format(c *wcCounts, name string) string {
	var parts []string
	if w.c.lines {
		parts = append(parts, fmt.Sprintf("%8d", c.l))
	}
	if w.c.words {
		parts = append(parts, fmt.Sprintf("%8d", c.w))
	}
	if w.c.chars {
		parts = append(parts, fmt.Sprintf("%8d", c.m))
	}
	if w.c.bytes {
		parts = append(parts, fmt.Sprintf("%8d", c.c))
	}
	s := strings.Join(parts, " ")
	if name != "" {
		s += " " + name
	}
	return s
}

func (w *wcStage) emitAll(emit Emit) {
	if !w.c.perFile {
		emit(Line{Text: w.format(&w.total, "")})
		return
	}
	names := make([]string, 0, len(w.per))
	for n := range w.per {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !emit(Line{Text: w.format(w.per[n], n)}) {
			return
		}
	}
	if len(names) != 1 {
		emit(Line{Text: w.format(&w.total, "total")})
	}
}

func (w *wcStage) Snapshot(emit Emit) { w.emitAll(emit) }
func (w *wcStage) Finish(emit Emit)   { w.emitAll(emit) }

// ---------------- head / tail ----------------

type headStage struct{ n, seen int }

func (h *headStage) Push(l Line, emit Emit) bool {
	if h.seen >= h.n {
		return false
	}
	h.seen++
	return emit(l) && h.seen < h.n
}
func (h *headStage) Finish(Emit) {}

type tailStage struct {
	n    int
	ring []Line
	pos  int
	full bool
}

func (t *tailStage) Push(l Line, emit Emit) bool {
	if t.n == 0 {
		return true
	}
	if t.ring == nil {
		t.ring = make([]Line, t.n)
	}
	t.ring[t.pos] = l
	t.pos++
	if t.pos == t.n {
		t.pos, t.full = 0, true
	}
	return true
}

func (t *tailStage) emitAll(emit Emit) {
	if t.full {
		for i := 0; i < t.n; i++ {
			if !emit(t.ring[(t.pos+i)%t.n]) {
				return
			}
		}
		return
	}
	for i := 0; i < t.pos; i++ {
		if !emit(t.ring[i]) {
			return
		}
	}
}

func (t *tailStage) Snapshot(emit Emit) { t.emitAll(emit) }
func (t *tailStage) Finish(emit Emit)   { t.emitAll(emit) }

// tail -n +N: everything from line N on.
type tailFromStage struct{ from, seen int }

func (t *tailFromStage) Push(l Line, emit Emit) bool {
	t.seen++
	if t.seen < t.from {
		return true
	}
	return emit(l)
}
func (t *tailFromStage) Finish(Emit) {}

// ---------------- cat ----------------

type passStage struct{}

func (passStage) Push(l Line, emit Emit) bool { return emit(l) }
func (passStage) Finish(Emit)                 {}

type catNumStage struct{ n int }

func (c *catNumStage) Push(l Line, emit Emit) bool {
	c.n++
	return emit(Line{Text: fmt.Sprintf("%6d\t%s", c.n, l.Text), Src: l.Src})
}
func (c *catNumStage) Finish(Emit) {}

// ---------------- cut ----------------

type cutRange struct{ lo, hi int } // 1-based inclusive, hi=0 means open

type cutCfg struct {
	byChar bool
	delim  string
	ranges []cutRange
	onlyD  bool
}

func parseRanges(s string) ([]cutRange, error) {
	var out []cutRange
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		r := cutRange{}
		var err error
		if lo != "" {
			if r.lo, err = strconv.Atoi(lo); err != nil || r.lo < 1 {
				return nil, fmt.Errorf("invalid range: %q", part)
			}
		} else {
			r.lo = 1
		}
		if !isRange {
			r.hi = r.lo
		} else if hi != "" {
			if r.hi, err = strconv.Atoi(hi); err != nil || r.hi < r.lo {
				return nil, fmt.Errorf("invalid range: %q", part)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func (c *cutCfg) selected(i int) bool { // i is 1-based
	for _, r := range c.ranges {
		if i >= r.lo && (r.hi == 0 || i <= r.hi) {
			return true
		}
	}
	return false
}

type cutStage struct{ c *cutCfg }

func (s cutStage) Push(l Line, emit Emit) bool {
	c := s.c
	var out string
	if c.byChar {
		var b strings.Builder
		i := 0
		for _, r := range l.Text {
			i++
			if c.selected(i) {
				b.WriteRune(r)
			}
		}
		out = b.String()
	} else {
		if !strings.Contains(l.Text, c.delim) {
			if c.onlyD {
				return true
			}
			return emit(l)
		}
		f := strings.Split(l.Text, c.delim)
		sel := make([]string, 0, len(f))
		for i, x := range f {
			if c.selected(i + 1) {
				sel = append(sel, x)
			}
		}
		out = strings.Join(sel, c.delim)
	}
	return emit(Line{Text: out, Src: l.Src, No: l.No})
}
func (cutStage) Finish(Emit) {}

// ---------------- tr ----------------

type trCfg struct {
	del, squeeze bool
	set1, set2   []rune
	inSet1       map[rune]bool
	mapping      map[rune]rune
	squeezeSet   map[rune]bool
}

func expandTrSet(s string) []rune {
	classes := map[string]func(rune) bool{
		"upper": func(r rune) bool { return r >= 'A' && r <= 'Z' },
		"lower": func(r rune) bool { return r >= 'a' && r <= 'z' },
		"digit": func(r rune) bool { return r >= '0' && r <= '9' },
		"alpha": func(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') },
		"alnum": func(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') },
		"space": func(r rune) bool { return strings.ContainsRune(" \t\n\r\v\f", r) },
		"punct": func(r rune) bool {
			return r > 32 && r < 127 && !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		},
	}
	// decode escapes first
	var rs []rune
	src := []rune(s)
	for i := 0; i < len(src); i++ {
		if src[i] == '[' && i+1 < len(src) && src[i+1] == ':' {
			if end := strings.Index(string(src[i:]), ":]"); end > 0 {
				name := string(src[i+2 : i+end])
				if f, ok := classes[name]; ok {
					for r := rune(0); r < 128; r++ {
						if f(r) {
							rs = append(rs, r)
						}
					}
					i += len([]rune(string(src[i:i+end]))) + 1
					continue
				}
			}
		}
		r := src[i]
		if r == '\\' && i+1 < len(src) {
			i++
			switch src[i] {
			case 'n':
				r = '\n'
			case 't':
				r = '\t'
			case 'r':
				r = '\r'
			default:
				r = src[i]
			}
		}
		if i+2 < len(src) && src[i+1] == '-' {
			hi := src[i+2]
			for x := r; x <= hi; x++ {
				rs = append(rs, x)
			}
			i += 2
			continue
		}
		rs = append(rs, r)
	}
	return rs
}

type trStage struct{ c *trCfg }

func (t trStage) Push(l Line, emit Emit) bool {
	c := t.c
	var b strings.Builder
	var last rune = -1
	for _, r := range l.Text {
		if c.del && c.inSet1[r] {
			continue
		}
		if m, ok := c.mapping[r]; ok {
			r = m
		}
		if c.squeeze && c.squeezeSet[r] && r == last {
			continue
		}
		b.WriteRune(r)
		last = r
	}
	return emit(Line{Text: b.String(), Src: l.Src, No: l.No})
}
func (trStage) Finish(Emit) {}

// ---------------- sed ----------------

type sedCmd struct {
	addr   *regexp.Regexp
	kind   byte // 's', 'd', 'p'
	re     *regexp.Regexp
	repl   string
	global bool
	print  bool
}

type sedCfg struct {
	quiet bool
	cmds  []sedCmd
}

type sedStage struct{ c *sedCfg }

func (s sedStage) Push(l Line, emit Emit) bool {
	text := l.Text
	mk := func(t string) Line { return Line{Text: t, Src: l.Src, No: l.No} }
	for i := range s.c.cmds {
		cmd := &s.c.cmds[i]
		if cmd.addr != nil && !cmd.addr.MatchString(text) {
			continue
		}
		switch cmd.kind {
		case 'd':
			return true
		case 'p':
			if !emit(mk(text)) {
				return false
			}
		case 's':
			loc := cmd.re.FindStringSubmatchIndex(text)
			if loc == nil {
				continue
			}
			if cmd.global {
				text = cmd.re.ReplaceAllString(text, cmd.repl)
			} else {
				dst := cmd.re.ExpandString(nil, cmd.repl, text, loc)
				text = text[:loc[0]] + string(dst) + text[loc[1]:]
			}
			if cmd.print && !emit(mk(text)) {
				return false
			}
		}
	}
	if s.c.quiet {
		return true
	}
	return emit(mk(text))
}
func (sedStage) Finish(Emit) {}

// sedReplacement converts sed replacement syntax (&, \1) to Go's ($0, ${1}).
func sedReplacement(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\' && i+1 < len(rs):
			n := rs[i+1]
			i++
			switch {
			case n >= '0' && n <= '9':
				b.WriteString("${" + string(n) + "}")
			case n == 'n':
				b.WriteByte('\n')
			case n == 't':
				b.WriteByte('\t')
			case n == '$':
				b.WriteString("$$")
			default:
				b.WriteRune(n)
			}
		case c == '&':
			b.WriteString("${0}")
		case c == '$':
			b.WriteString("$$")
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// splitDelim reads text up to an unescaped delimiter; returns the part and the rest.
func splitDelim(s string, d rune) (string, string, bool) {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		if rs[i] == '\\' && i+1 < len(rs) {
			if rs[i+1] == d {
				b.WriteRune(d)
			} else {
				b.WriteRune('\\')
				b.WriteRune(rs[i+1])
			}
			i++
			continue
		}
		if rs[i] == d {
			return b.String(), string(rs[i+1:]), true
		}
		b.WriteRune(rs[i])
	}
	return b.String(), "", false
}

func parseSedScript(script string, extended bool) ([]sedCmd, error) {
	compile := func(p string, icase bool) (*regexp.Regexp, error) {
		if !extended {
			p = breToEre(p)
		}
		if icase {
			p = "(?i)" + p
		}
		return regexp.Compile(p)
	}
	var cmds []sedCmd
	for _, part := range strings.Split(script, "\n") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		var cmd sedCmd
		if strings.HasPrefix(s, "/") {
			pat, rest, ok := splitDelim(s[1:], '/')
			if !ok {
				return nil, fmt.Errorf("sed: unterminated address /%s", pat)
			}
			re, err := compile(pat, false)
			if err != nil {
				return nil, fmt.Errorf("sed: %v", err)
			}
			cmd.addr = re
			s = strings.TrimSpace(rest)
		}
		if s == "" {
			return nil, fmt.Errorf("sed: missing command")
		}
		switch s[0] {
		case 'd', 'p':
			if len(strings.TrimSpace(s[1:])) > 0 {
				return nil, fmt.Errorf("sed: unknown command: %q", s)
			}
			cmd.kind = s[0]
		case 's':
			rs := []rune(s)
			if len(rs) < 2 {
				return nil, fmt.Errorf("sed: malformed s command")
			}
			d := rs[1]
			pat, rest, ok1 := splitDelim(string(rs[2:]), d)
			repl, flags, ok2 := splitDelim(rest, d)
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("sed: unterminated s command: %q", s)
			}
			icase := false
			for _, f := range flags {
				switch f {
				case 'g':
					cmd.global = true
				case 'p':
					cmd.print = true
				case 'i', 'I':
					icase = true
				default:
					return nil, fmt.Errorf("sed: unknown flag %q", f)
				}
			}
			re, err := compile(pat, icase)
			if err != nil {
				return nil, fmt.Errorf("sed: %v", err)
			}
			cmd.kind, cmd.re, cmd.repl = 's', re, sedReplacement(repl)
		default:
			return nil, fmt.Errorf("sed: unsupported command: %q (only s, d, p)", s)
		}
		cmds = append(cmds, cmd)
	}
	return cmds, nil
}

// ---------------- static source ----------------

type staticStage struct{}

func (staticStage) Push(l Line, emit Emit) bool { return emit(l) }
func (staticStage) Finish(Emit)                 {}

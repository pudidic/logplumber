package main

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SPL search language: the first part of an SPL query and the search command.
//
//	error                     the term appears in _raw (case-insensitive, whole words)
//	"db timeout"              a phrase
//	fail*                     wildcards
//	status=500 status>=500    field comparisons (values case-insensitive, wildcards with =)
//	status!=200               the field exists and differs
//	user=*                    the field exists
//	level IN (warn, error)    one of several values
//	NOT a   a OR b   a AND b  boolean operators (uppercase); terms side by side mean AND;
//	                          as in Splunk, OR binds tighter than AND
//	earliest=-24h latest=now  time range
//	source=*.jsonl            files to read (by path or name, archive members too)
//	index=...                 accepted and ignored: psh has no indexes

type searchCtx struct {
	l     *Line
	lower string
	low   bool
}

func (c *searchCtx) raw() string {
	if !c.low {
		c.lower, c.low = strings.ToLower(c.l.Text), true
	}
	return c.lower
}

type snode interface{ match(c *searchCtx) bool }

type sAnd []snode
type sOr []snode
type sNot struct{ x snode }
type sAll struct{}

func (n sAnd) match(c *searchCtx) bool {
	for _, x := range n {
		if !x.match(c) {
			return false
		}
	}
	return true
}

func (n sOr) match(c *searchCtx) bool {
	for _, x := range n {
		if x.match(c) {
			return true
		}
	}
	return false
}

func (n sNot) match(c *searchCtx) bool { return !n.x.match(c) }
func (sAll) match(*searchCtx) bool     { return true }

// sTerm matches a word or phrase in _raw, bounded by non-word characters.
type sTerm struct {
	lit string         // lowercased literal (no wildcards)
	re  *regexp.Regexp // with wildcards
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func (n sTerm) match(c *searchCtx) bool {
	if n.re != nil {
		return n.re.MatchString(c.l.Text)
	}
	s := c.raw()
	for from := 0; ; {
		i := strings.Index(s[from:], n.lit)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(n.lit)
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[end:])
		if (i == 0 || !isWordRune(before) || !isWordRune(rune(n.lit[0]))) &&
			(end == len(s) || !isWordRune(after) || !isWordRune(rune(n.lit[len(n.lit)-1]))) {
			return true
		}
		from = i + 1
	}
}

func newTerm(s string) snode {
	if s == "*" {
		return sAll{}
	}
	if !strings.Contains(s, "*") {
		return sTerm{lit: strings.ToLower(s)}
	}
	var b strings.Builder
	b.WriteString(`(?is)(?:^|[^\pL\pN_])`)
	for i, part := range strings.Split(s, "*") {
		if i > 0 {
			b.WriteString(`.*?`)
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString(`(?:$|[^\pL\pN_])`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return sTerm{lit: strings.ToLower(s)}
	}
	return sTerm{re: re}
}

// sCmp compares a field with a value.
type sCmp struct {
	field, op string
	val       value
	pat       *regexp.Regexp // "=" or "!=" with wildcards
	exists    bool           // field=*
	lit       string         // "=": lowercased text that _raw must contain
}

func (n sCmp) match(c *searchCtx) bool {
	if n.lit != "" && !strings.Contains(c.raw(), n.lit) {
		return false // cheap check before extracting fields
	}
	v, ok := c.l.Field(n.field)
	if !ok {
		return false
	}
	if n.exists {
		return n.op == "="
	}
	switch n.op {
	case "=", "!=":
		eq := false
		if n.pat != nil {
			eq = n.pat.MatchString(v)
		} else {
			fv := strV(v)
			if fv.num && n.val.num {
				eq = fv.n == n.val.n
			} else {
				eq = strings.EqualFold(v, n.val.s)
			}
		}
		return eq == (n.op == "=")
	}
	r, _ := compare(strV(v), n.val)
	switch n.op {
	case "<":
		return r < 0
	case "<=":
		return r <= 0
	case ">":
		return r > 0
	}
	return r >= 0
}

func newCmp(field, op, val string) sCmp {
	n := sCmp{field: field, op: op, val: strV(val)}
	if n.val.s == "" {
		n.val.s = val
	}
	if op == "=" || op == "!=" {
		if val == "*" {
			n.exists = true
			return n
		}
		if strings.Contains(val, "*") {
			var b strings.Builder
			b.WriteString("(?is)^")
			for i, part := range strings.Split(val, "*") {
				if i > 0 {
					b.WriteString(".*")
				}
				b.WriteString(regexp.QuoteMeta(part))
			}
			b.WriteString("$")
			n.pat = regexp.MustCompile(b.String())
		} else if op == "=" && !n.val.num && !strings.HasPrefix(field, "_") && plainText(val) {
			n.lit = strings.ToLower(val)
		}
	}
	return n
}

// plainText reports whether s appears unchanged in JSON and key=value text, so
// a field holding it must contain it literally in _raw.
func plainText(s string) bool {
	for _, r := range s {
		if r < 0x20 || r >= 0x7f || r == '"' || r == '\\' || r == '/' {
			return false
		}
	}
	return s != ""
}

// sTime keeps events whose _time is in a range.
type sTime struct{ r timeRange }

func (n sTime) match(c *searchCtx) bool {
	ms, ok := c.l.lineTime()
	return ok && n.r.contains(ms)
}

// ---------------- parser ----------------

type stok struct {
	s      string
	pos    int
	quoted bool
	op     bool // "(", ")", ",", or a comparison operator
}

// lexSearch splits search text into words, quoted strings, parentheses and
// comparison operators. base is the rune offset of src in the input line.
func lexSearch(src string, base int) ([]stok, error) {
	rs := []rune(src)
	var out []stok
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) && (rs[j+1] == '"' || rs[j+1] == '\\') {
					j++
				}
				b.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return nil, errAt(base+i, "unterminated quote")
			}
			out = append(out, stok{s: b.String(), pos: base + i, quoted: true})
			i = j + 1
		case c == '(' || c == ')' || c == ',':
			out = append(out, stok{s: string(c), pos: base + i, op: true})
			i++
		case c == '=' || c == '<' || c == '>' || c == '!' && i+1 < len(rs) && rs[i+1] == '=':
			j := i + 1
			if j < len(rs) && rs[j] == '=' {
				j++
			}
			op := string(rs[i:j])
			if op == "==" {
				op = "="
			}
			out = append(out, stok{s: op, pos: base + i, op: true})
			i = j
		default:
			j := i
			for j < len(rs) && !unicode.IsSpace(rs[j]) && !strings.ContainsRune(`"(),=<>`, rs[j]) &&
				!(rs[j] == '!' && j+1 < len(rs) && rs[j+1] == '=') {
				j++
			}
			out = append(out, stok{s: string(rs[i:j]), pos: base + i})
			i = j
		}
	}
	return out, nil
}

type searchParser struct {
	toks    []stok
	i       int
	end     int // rune offset of the end of the text, for errors at the end
	now     time.Time
	sources []string // source= patterns at the top level (files to read)
	depth   int      // inside parentheses, NOT or OR
	// selectFiles: top-level source= terms choose the files to read (first search only)
	selectFiles bool
}

func (p *searchParser) peek() *stok {
	if p.i < len(p.toks) {
		return &p.toks[p.i]
	}
	return nil
}

func (p *searchParser) isKw(kw string) bool {
	t := p.peek()
	return t != nil && !t.quoted && !t.op && t.s == kw
}

func (p *searchParser) isOp(op string) bool {
	t := p.peek()
	return t != nil && t.op && t.s == op
}

func (p *searchParser) errHere(format string, a ...any) error {
	if t := p.peek(); t != nil {
		return errAt(t.pos, format, a...)
	}
	return errAt(p.end, format, a...)
}

// parseSearchText parses the text of a search into a matcher. It also returns the
// source= patterns that select the files to read.
func parseSearchText(src string, base int, selectFiles bool) (snode, []string, error) {
	toks, err := lexSearch(src, base)
	if err != nil {
		return nil, nil, err
	}
	p := &searchParser{toks: toks, end: base + len([]rune(src)), now: time.Now(), selectFiles: selectFiles}
	if len(toks) == 0 {
		return sAll{}, nil, nil
	}
	n, err := p.parseAnd()
	if err != nil {
		return nil, nil, err
	}
	if t := p.peek(); t != nil {
		return nil, nil, errAt(t.pos, "unexpected %q", t.s)
	}
	return n, p.sources, nil
}

func (p *searchParser) parseAnd() (snode, error) {
	var out sAnd
	var tr timeRange
	hasTime, seen := false, false
	for {
		if p.isKw("AND") {
			if len(out) == 0 && !hasTime {
				return nil, p.errHere("search term expected before AND")
			}
			p.i++
		}
		t := p.peek()
		if t == nil || t.op && t.s == ")" || p.isKw("OR") {
			break
		}
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		seen = true
		if st, ok := n.(sTime); ok && p.depth == 0 { // merge earliest/latest into one range
			hasTime = true
			if st.r.hasE {
				tr.earliest, tr.hasE = st.r.earliest, true
			}
			if st.r.hasL {
				tr.latest, tr.hasL = st.r.latest, true
			}
			continue
		}
		if n != nil {
			out = append(out, n)
		}
	}
	if !seen {
		return nil, p.errHere("search term expected")
	}
	if len(out) == 0 && !hasTime {
		return sAll{}, nil // only source= or index= terms
	}
	// cheap checks first: the time range, then words in _raw, then fields
	sort.SliceStable(out, func(i, j int) bool { return cost(out[i]) < cost(out[j]) })
	if hasTime {
		out = append(sAnd{sTime{r: tr}}, out...)
	}
	if len(out) == 1 {
		return out[0], nil
	}
	return out, nil
}

func cost(n snode) int {
	switch n := n.(type) {
	case sTerm:
		if n.re == nil {
			return 0
		}
		return 1
	case sCmp:
		if n.lit != "" {
			return 2
		}
	}
	return 3
}

func (p *searchParser) parseOr() (snode, error) {
	n0 := len(p.sources)
	a, err := p.parseNot()
	if err != nil || !p.isKw("OR") {
		return a, err
	}
	if len(p.sources) > n0 { // "source=x OR ...": a filter on _src, not a file choice
		a = newCmp("_src", "=", p.sources[n0])
		p.sources = p.sources[:n0]
	}
	out := sOr{a}
	p.depth++
	defer func() { p.depth-- }()
	for p.isKw("OR") {
		p.i++
		b, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	for i, x := range out {
		if x == nil { // a source= or index= term inside OR
			out[i] = sAll{}
		}
	}
	return out, nil
}

func (p *searchParser) parseNot() (snode, error) {
	if p.isKw("NOT") {
		p.i++
		p.depth++
		x, err := p.parseNot()
		p.depth--
		if err != nil {
			return nil, err
		}
		if x == nil {
			x = sAll{}
		}
		return sNot{x}, nil
	}
	return p.parsePrimary()
}

func (p *searchParser) parsePrimary() (snode, error) {
	t := p.peek()
	if t == nil {
		return nil, p.errHere("search term expected")
	}
	if t.op {
		if t.s != "(" {
			return nil, errAt(t.pos, "search term expected, found %q", t.s)
		}
		p.i++
		p.depth++
		n, err := p.parseAnd()
		p.depth--
		if err != nil {
			return nil, err
		}
		if !p.isOp(")") {
			return nil, p.errHere("')' expected")
		}
		p.i++
		return n, nil
	}
	p.i++
	if t.quoted {
		return newTerm(t.s), nil
	}
	// field IN (a, b)
	if p.isKw("IN") {
		p.i++
		if !p.isOp("(") {
			return nil, p.errHere("'(' expected after IN")
		}
		p.i++
		var or sOr
		for {
			v := p.peek()
			if v == nil || v.op {
				return nil, p.errHere("value expected")
			}
			p.i++
			or = append(or, newCmp(t.s, "=", v.s))
			if p.isOp(",") {
				p.i++
				continue
			}
			if !p.isOp(")") {
				return nil, p.errHere("',' or ')' expected")
			}
			p.i++
			return or, nil
		}
	}
	// field op value
	if op := p.peek(); op != nil && op.op && strings.ContainsAny(op.s, "=<>") {
		p.i++
		v := p.peek()
		if v == nil || v.op {
			return nil, errAt(op.pos, "value expected after %s%s", t.s, op.s)
		}
		p.i++
		return p.comparison(t, op.s, v)
	}
	return newTerm(t.s), nil
}

func (p *searchParser) comparison(f *stok, op string, v *stok) (snode, error) {
	switch strings.ToLower(f.s) {
	case "earliest", "latest":
		if op != "=" {
			return nil, errAt(f.pos, "%s takes '=', e.g. %s=-24h", f.s, f.s)
		}
		tm, err := parseTimeSpec(v.s, p.now)
		if err != nil {
			return nil, errAt(v.pos, "%v", err)
		}
		var r timeRange
		if strings.EqualFold(f.s, "earliest") {
			r.earliest, r.hasE = tm.UnixMilli(), true
		} else {
			r.latest, r.hasL = tm.UnixMilli(), true
		}
		return sTime{r: r}, nil
	case "index":
		return nil, nil // psh has no indexes: every event matches
	case "source":
		if op == "=" && p.depth == 0 && p.selectFiles {
			p.sources = append(p.sources, v.s)
			return nil, nil
		}
		return newCmp("_src", op, v.s), nil
	}
	return newCmp(f.s, op, v.s), nil
}

// ---------------- search command ----------------

type searchStage struct{ n snode }

func (s searchStage) Push(l Line, emit Emit) bool {
	if s.n.match(&searchCtx{l: &l}) {
		return emit(l)
	}
	return true
}
func (searchStage) Finish(Emit) {}

type timeRange struct {
	earliest, latest int64 // Unix milliseconds; earliest inclusive, latest exclusive
	hasE, hasL       bool
}

func (r timeRange) contains(ms int64) bool {
	return (!r.hasE || ms >= r.earliest) && (!r.hasL || ms < r.latest)
}

// parseSearchCmd parses the text of a search. sources are the source= patterns
// that select files; they only apply to the first search of a query.
func parseSearchCmd(src string, pos int, first bool) (*Spec, []string, error) {
	n, sources, err := parseSearchText(src, pos, first)
	if err != nil {
		return nil, nil, err
	}
	if _, all := n.(sAll); all || n == nil {
		return &Spec{Name: "search", Parallel: true, New: func() Stage { return passStage{} }}, sources, nil
	}
	return &Spec{Name: "search", Parallel: true, New: func() Stage { return searchStage{n: n} }}, sources, nil
}

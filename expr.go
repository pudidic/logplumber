package main

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Expression language of where/eval:
//
//	or  := and { OR and }          and := not { AND not }        not := NOT not | cmp
//	cmp := add [ (= == != < <= > >=) add | [NOT] IN (list) | [NOT] LIKE add ]
//	add := mul { (+ | -) mul }     mul := unary { (* | / | %) unary }
//	unary := - unary | primary
//	primary := number | "string" | field | 'field name' | func(args) | ( or )
//
// Keywords are case-insensitive. Field values are strings that compare numerically
// when both sides are numbers. A missing field is null; comparisons with null are false.

// ---------------- values ----------------

type value struct {
	s    string
	n    float64
	num  bool // n is valid
	null bool
	b    bool
	bool bool // value is a boolean
}

var nullV = value{null: true}

func strV(s string) value {
	if f, ok := parseNum(s); ok {
		return value{s: s, n: f, num: true}
	}
	return value{s: s}
}

func numV(f float64) value {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nullV
	}
	return value{n: f, num: true}
}

func boolV(b bool) value { return value{b: b, bool: true} }

func (v value) String() string {
	switch {
	case v.null:
		return ""
	case v.bool:
		return strconv.FormatBool(v.b)
	case v.s != "" || !v.num:
		return v.s
	}
	return formatNum(v.n)
}

func (v value) truthy() bool {
	switch {
	case v.null:
		return false
	case v.bool:
		return v.b
	case v.num:
		return v.n != 0
	}
	return v.s != ""
}

func compare(a, b value) (int, bool) {
	if a.null || b.null {
		return 0, false
	}
	if a.num && b.num {
		switch {
		case a.n < b.n:
			return -1, true
		case a.n > b.n:
			return 1, true
		}
		return 0, true
	}
	return strings.Compare(a.String(), b.String()), true
}

// ---------------- lexer ----------------

type tokKind int

const (
	tEOF tokKind = iota
	tNum
	tStr
	tIdent // field, function or keyword
	tField // 'quoted field name'
	tOp
)

type exprTok struct {
	kind tokKind
	s    string
	pos  int // rune offset in the input line
}

// lexExpr tokenizes src; base is the rune offset of src in the input line.
func lexExpr(src string, base int) ([]exprTok, error) {
	rs := []rune(src)
	var out []exprTok
	for i := 0; i < len(rs); {
		c := rs[i]
		pos := base + i
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '"' || c == '\'':
			var b strings.Builder
			j := i + 1
			for ; j < len(rs) && rs[j] != c; j++ {
				if rs[j] == '\\' && j+1 < len(rs) && (rs[j+1] == c || rs[j+1] == '\\') {
					j++
				}
				b.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return nil, errAt(pos, "unterminated quote")
			}
			k := tStr
			if c == '\'' {
				k = tField
			}
			out = append(out, exprTok{kind: k, s: b.String(), pos: pos})
			i = j + 1
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(rs) && rs[i+1] >= '0' && rs[i+1] <= '9':
			j := i
			for j < len(rs) && (rs[j] >= '0' && rs[j] <= '9' || rs[j] == '.' ||
				(rs[j] == 'e' || rs[j] == 'E') && j+1 < len(rs) && (rs[j+1] >= '0' && rs[j+1] <= '9' || rs[j+1] == '-' || rs[j+1] == '+') ||
				(rs[j] == '-' || rs[j] == '+') && j > i && (rs[j-1] == 'e' || rs[j-1] == 'E')) {
				j++
			}
			out = append(out, exprTok{kind: tNum, s: string(rs[i:j]), pos: pos})
			i = j
		case c == '_' || unicode.IsLetter(c):
			j := i
			for j < len(rs) && (rs[j] == '_' || rs[j] == '.' || unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j])) {
				j++
			}
			out = append(out, exprTok{kind: tIdent, s: string(rs[i:j]), pos: pos})
			i = j
		default:
			two := ""
			if i+1 < len(rs) {
				two = string(rs[i : i+2])
			}
			switch two {
			case "==", "!=", "<=", ">=", "<>":
				out = append(out, exprTok{kind: tOp, s: two, pos: pos})
				i += 2
				continue
			}
			if !strings.ContainsRune("=<>+-*/%(),!", c) {
				return nil, errAt(pos, "unexpected character %q", c)
			}
			out = append(out, exprTok{kind: tOp, s: string(c), pos: pos})
			i++
		}
	}
	out = append(out, exprTok{kind: tEOF, pos: base + len(rs)})
	return out, nil
}

// ---------------- AST ----------------

type node interface{ eval(l *Line) value }

type litNode struct{ v value }
type fieldNode struct{ name string }
type unaryNode struct {
	op string
	x  node
}
type binNode struct {
	op   string
	a, b node
}
type inNode struct {
	x    node
	list []node
	not  bool
}
type callNode struct {
	name string
	args []node
	re   *regexp.Regexp // pre-compiled when the pattern is a literal
}

func (n litNode) eval(*Line) value { return n.v }

func (n fieldNode) eval(l *Line) value {
	v, ok := l.Field(n.name)
	if !ok {
		return nullV
	}
	return strV(v)
}

func (n unaryNode) eval(l *Line) value {
	v := n.x.eval(l)
	switch n.op {
	case "not":
		if v.null {
			return nullV
		}
		return boolV(!v.truthy())
	case "-":
		if !v.num {
			return nullV
		}
		return numV(-v.n)
	}
	return nullV
}

func (n binNode) eval(l *Line) value {
	switch n.op {
	case "and":
		return boolV(n.a.eval(l).truthy() && n.b.eval(l).truthy())
	case "or":
		return boolV(n.a.eval(l).truthy() || n.b.eval(l).truthy())
	}
	a, b := n.a.eval(l), n.b.eval(l)
	switch n.op {
	case "=", "==", "!=", "<>", "<", "<=", ">", ">=":
		c, ok := compare(a, b)
		if !ok {
			return nullV // a missing field neither matches nor fails to match
		}
		switch n.op {
		case "=", "==":
			return boolV(c == 0)
		case "!=", "<>":
			return boolV(c != 0)
		case "<":
			return boolV(c < 0)
		case "<=":
			return boolV(c <= 0)
		case ">":
			return boolV(c > 0)
		}
		return boolV(c >= 0)
	case "like", "notlike":
		if a.null || b.null {
			return boolV(false)
		}
		m := likeMatch(a.String(), b.String())
		return boolV(m == (n.op == "like"))
	case "+":
		if a.null || b.null {
			return nullV
		}
		if a.num && b.num {
			return numV(a.n + b.n)
		}
		return value{s: a.String() + b.String()}
	}
	if !a.num || !b.num {
		return nullV
	}
	switch n.op {
	case "-":
		return numV(a.n - b.n)
	case "*":
		return numV(a.n * b.n)
	case "/":
		return numV(a.n / b.n)
	case "%":
		return numV(math.Mod(a.n, b.n))
	}
	return nullV
}

func (n inNode) eval(l *Line) value {
	v := n.x.eval(l)
	if v.null {
		return boolV(false)
	}
	found := false
	for _, x := range n.list {
		if c, ok := compare(v, x.eval(l)); ok && c == 0 {
			found = true
			break
		}
	}
	return boolV(found != n.not)
}

// likeMatch implements SQL LIKE: % matches any run, _ one character.
func likeMatch(s, pat string) bool {
	var b strings.Builder
	b.WriteString("(?s)^")
	for _, r := range pat {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := cachedRegexp(b.String())
	return err == nil && re.MatchString(s)
}

var reCache sync.Map

func cachedRegexp(p string) (*regexp.Regexp, error) {
	if v, ok := reCache.Load(p); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	reCache.Store(p, re)
	return re, nil
}

// funcArity gives the allowed argument counts of each function (max -1 = variadic).
var funcArity = map[string][2]int{
	"if": {3, 3}, "case": {2, -1}, "coalesce": {1, -1}, "isnull": {1, 1}, "isnotnull": {1, 1},
	"isnum": {1, 1}, "len": {1, 1}, "lower": {1, 1}, "upper": {1, 1}, "trim": {1, 1},
	"substr": {2, 3}, "round": {1, 2}, "floor": {1, 1}, "ceil": {1, 1}, "abs": {1, 1},
	"tonumber": {1, 1}, "tostring": {1, 1}, "match": {2, 2}, "like": {2, 2}, "replace": {3, 3},
	"min": {1, -1}, "max": {1, -1}, "null": {0, 0}, "true": {0, 0}, "false": {0, 0},
}

func (n *callNode) regex(l *Line, i int) (*regexp.Regexp, bool) {
	if n.re != nil {
		return n.re, true
	}
	re, err := cachedRegexp(n.args[i].eval(l).String())
	return re, err == nil
}

func (n *callNode) eval(l *Line) value {
	arg := func(i int) value { return n.args[i].eval(l) }
	switch n.name {
	case "if":
		if arg(0).truthy() {
			return arg(1)
		}
		return arg(2)
	case "case":
		for i := 0; i+1 < len(n.args); i += 2 {
			if arg(i).truthy() {
				return arg(i + 1)
			}
		}
		return nullV
	case "coalesce":
		for i := range n.args {
			if v := arg(i); !v.null {
				return v
			}
		}
		return nullV
	case "isnull":
		return boolV(arg(0).null)
	case "isnotnull":
		return boolV(!arg(0).null)
	case "isnum":
		return boolV(arg(0).num)
	case "null":
		return nullV
	case "true":
		return boolV(true)
	case "false":
		return boolV(false)
	case "min", "max":
		var best value = nullV
		for i := range n.args {
			v := arg(i)
			if v.null {
				continue
			}
			if c, _ := compare(v, best); best.null || (n.name == "min" && c < 0) || (n.name == "max" && c > 0) {
				best = v
			}
		}
		return best
	}
	v := arg(0)
	if v.null {
		return nullV
	}
	switch n.name {
	case "len":
		return numV(float64(len([]rune(v.String()))))
	case "lower":
		return value{s: strings.ToLower(v.String())}
	case "upper":
		return value{s: strings.ToUpper(v.String())}
	case "trim":
		return strV(strings.TrimSpace(v.String()))
	case "tostring":
		return value{s: v.String()}
	case "tonumber":
		if f, ok := parseNum(v.String()); ok {
			return numV(f)
		}
		return nullV
	case "substr":
		rs := []rune(v.String())
		start := int(arg(1).n)
		if start < 1 {
			start = 1
		}
		if start > len(rs) {
			return value{}
		}
		end := len(rs)
		if len(n.args) == 3 {
			end = min(end, start-1+int(arg(2).n))
		}
		return strV(string(rs[start-1 : max(start-1, end)]))
	case "round":
		if !v.num {
			return nullV
		}
		d := 0.0
		if len(n.args) == 2 {
			d = arg(1).n
		}
		p := math.Pow(10, d)
		return numV(math.Round(v.n*p) / p)
	case "floor", "ceil", "abs":
		if !v.num {
			return nullV
		}
		f := map[string]func(float64) float64{"floor": math.Floor, "ceil": math.Ceil, "abs": math.Abs}[n.name]
		return numV(f(v.n))
	case "match":
		re, ok := n.regex(l, 1)
		return boolV(ok && re.MatchString(v.String()))
	case "like":
		p := arg(1)
		return boolV(!p.null && likeMatch(v.String(), p.String()))
	case "replace":
		re, ok := n.regex(l, 1)
		if !ok {
			return nullV
		}
		return strV(re.ReplaceAllString(v.String(), sedReplacement(arg(2).String())))
	}
	return nullV
}

// ---------------- parser ----------------

type exprParser struct {
	toks []exprTok
	i    int
}

func (p *exprParser) peek() exprTok { return p.toks[p.i] }
func (p *exprParser) next() exprTok {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *exprParser) isKw(kw string) bool {
	t := p.peek()
	return t.kind == tIdent && strings.EqualFold(t.s, kw)
}

func (p *exprParser) isOp(op string) bool {
	t := p.peek()
	return t.kind == tOp && t.s == op
}

func (p *exprParser) expectOp(op string) error {
	if !p.isOp(op) {
		return p.errExpected("'" + op + "'")
	}
	p.next()
	return nil
}

// eofPos is where to report an error at the end of the input: the last token, so
// the dangling part (e.g. a trailing operator) is what gets underlined.
func (p *exprParser) eofPos() int {
	if p.i > 0 {
		return p.toks[p.i-1].pos
	}
	return p.peek().pos
}

func (p *exprParser) errExpected(what string) error {
	t := p.peek()
	if t.kind == tEOF {
		return errAt(p.eofPos(), "%s expected", what)
	}
	return errAt(t.pos, "%s expected, found %q", what, t.s)
}

// parseExpression parses a complete expression from src (at rune offset base).
func parseExpression(src string, base int) (node, error) {
	toks, err := lexExpr(src, base)
	if err != nil {
		return nil, err
	}
	p := &exprParser{toks: toks}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.errExpected("operator or end of expression")
	}
	return n, nil
}

func (p *exprParser) parseOr() (node, error) {
	a, err := p.parseAnd()
	for err == nil && p.isKw("or") {
		p.next()
		var b node
		if b, err = p.parseAnd(); err == nil {
			a = binNode{op: "or", a: a, b: b}
		}
	}
	return a, err
}

func (p *exprParser) parseAnd() (node, error) {
	a, err := p.parseNot()
	for err == nil && p.isKw("and") {
		p.next()
		var b node
		if b, err = p.parseNot(); err == nil {
			a = binNode{op: "and", a: a, b: b}
		}
	}
	return a, err
}

func (p *exprParser) parseNot() (node, error) {
	if p.isKw("not") || p.isOp("!") {
		p.next()
		x, err := p.parseNot()
		return unaryNode{op: "not", x: x}, err
	}
	return p.parseCmp()
}

func (p *exprParser) parseCmp() (node, error) {
	a, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind == tOp {
		switch t.s {
		case "=", "==", "!=", "<>", "<", "<=", ">", ">=":
			p.next()
			b, err := p.parseAdd()
			return binNode{op: t.s, a: a, b: b}, err
		}
	}
	not := false
	if p.isKw("not") && p.i+1 < len(p.toks) && p.toks[p.i+1].kind == tIdent &&
		(strings.EqualFold(p.toks[p.i+1].s, "in") || strings.EqualFold(p.toks[p.i+1].s, "like")) {
		p.next()
		not = true
	}
	switch {
	case p.isKw("in"):
		p.next()
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		n := inNode{x: a, not: not}
		for {
			v, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			n.list = append(n.list, v)
			if p.isOp(",") {
				p.next()
				continue
			}
			return n, p.expectOp(")")
		}
	case p.isKw("like"):
		p.next()
		b, err := p.parseAdd()
		op := "like"
		if not {
			op = "notlike"
		}
		return binNode{op: op, a: a, b: b}, err
	}
	return a, nil
}

func (p *exprParser) parseAdd() (node, error) {
	a, err := p.parseMul()
	for err == nil && (p.isOp("+") || p.isOp("-")) {
		op := p.next().s
		var b node
		if b, err = p.parseMul(); err == nil {
			a = binNode{op: op, a: a, b: b}
		}
	}
	return a, err
}

func (p *exprParser) parseMul() (node, error) {
	a, err := p.parseUnary()
	for err == nil && (p.isOp("*") || p.isOp("/") || p.isOp("%")) {
		op := p.next().s
		var b node
		if b, err = p.parseUnary(); err == nil {
			a = binNode{op: op, a: a, b: b}
		}
	}
	return a, err
}

func (p *exprParser) parseUnary() (node, error) {
	if p.isOp("-") {
		p.next()
		x, err := p.parseUnary()
		return unaryNode{op: "-", x: x}, err
	}
	return p.parsePrimary()
}

var exprKeywords = map[string]bool{"and": true, "or": true, "not": true, "in": true, "like": true}

func (p *exprParser) parsePrimary() (node, error) {
	t := p.peek()
	switch t.kind {
	case tNum:
		p.next()
		f, err := strconv.ParseFloat(t.s, 64)
		if err != nil {
			return nil, errAt(t.pos, "invalid number %q", t.s)
		}
		return litNode{v: value{s: t.s, n: f, num: true}}, nil
	case tStr:
		p.next()
		return litNode{v: value{s: t.s}}, nil
	case tField:
		p.next()
		return fieldNode{name: t.s}, nil
	case tIdent:
		low := strings.ToLower(t.s)
		if exprKeywords[low] {
			return nil, errAt(t.pos, "value expected, found %q", t.s)
		}
		p.next()
		if !p.isOp("(") {
			return fieldNode{name: t.s}, nil
		}
		ar, ok := funcArity[low]
		if !ok {
			return nil, errAt(t.pos, "unknown function %s()", t.s)
		}
		p.next()
		n := &callNode{name: low}
		if !p.isOp(")") {
			for {
				a, err := p.parseOr()
				if err != nil {
					return nil, err
				}
				n.args = append(n.args, a)
				if !p.isOp(",") {
					break
				}
				p.next()
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		if len(n.args) < ar[0] || ar[1] >= 0 && len(n.args) > ar[1] {
			return nil, errAt(t.pos, "%s() takes %s argument(s)", low, arityText(ar))
		}
		if low == "match" || low == "replace" {
			if lit, ok := n.args[1].(litNode); ok {
				re, err := regexp.Compile(lit.v.String())
				if err != nil {
					return nil, errAt(t.pos, "%s(): invalid regex: %v", low, err)
				}
				n.re = re
			}
		}
		return n, nil
	case tOp:
		if t.s == "(" {
			p.next()
			x, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			return x, p.expectOp(")")
		}
	}
	return nil, p.errExpected("value")
}

func arityText(a [2]int) string {
	switch {
	case a[1] < 0:
		return strconv.Itoa(a[0]) + " or more"
	case a[0] == a[1]:
		return strconv.Itoa(a[0])
	}
	return strconv.Itoa(a[0]) + "-" + strconv.Itoa(a[1])
}

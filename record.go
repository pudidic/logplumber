package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Record holds the fields of a line. Fields come from three places: automatic
// extraction (JSON or key=value text, parsed lazily on first access), explicit
// extraction or computation (rex, eval), and aggregation output (stats, top, table).
type Record struct {
	set    map[string]string // fields set by rex/eval or produced by an aggregation
	auto   map[string]string // automatically extracted fields
	parsed bool
	row    bool         // produced by an aggregation: no automatic extraction
	header *tableHeader // column header shared by the rows of one table
	tm     int64        // _time in Unix milliseconds, valid when tState == 1
	tState uint8        // 0 not looked for yet, 1 found, 2 none
	order  []string     // names set by SetField, in order (new table columns)

	// fields and rename (SPL)
	filtered bool
	keep     []string          // "fields a, b*": only these (and _internal) fields; nil = all
	drop     []string          // "fields - a": patterns of removed fields
	hidden   map[string]bool   // renamed away
	renamed  map[string]string // old name -> new name
}

// visible reports whether fields/rename leave a field in place.
func (r *Record) visible(name string) bool {
	if !r.filtered {
		return true
	}
	if r.hidden[name] || matchFieldPats(r.drop, name) {
		return false
	}
	return r.keep == nil || strings.HasPrefix(name, "_") || matchFieldPats(r.keep, name)
}

type tableHeader struct {
	cols  []string
	text  string
	rule  string
	align []bool // right-align (numeric) per column
}

// posError is an error at a rune offset of the input line, so the type-ahead editor
// can underline exactly the bad part.
type posError struct {
	pos int
	msg string
}

func (e *posError) Error() string { return e.msg }

func (l *Line) rec() *Record {
	if l.Rec == nil {
		l.Rec = &Record{}
	}
	return l.Rec
}

// Field returns the value of a field and whether it exists.
func (l *Line) Field(name string) (string, bool) {
	if l.Rec != nil {
		if v, ok := l.Rec.set[name]; ok {
			return v, true
		}
		if !l.Rec.visible(name) {
			return "", false
		}
	}
	switch name {
	case "_raw":
		return l.Text, true
	case "_src":
		return l.Src, l.Src != ""
	case "_line":
		return strconv.Itoa(l.No), l.No > 0
	case "_time":
		ms, ok := l.lineTime()
		if !ok {
			return "", false
		}
		return epochString(ms), true
	}
	r := l.rec()
	if r.row {
		return "", false
	}
	if !r.parsed {
		r.auto = autoFields(l.Text)
		r.parsed = true
	}
	v, ok := r.auto[name]
	return v, ok
}

func (l *Line) SetField(name, v string) {
	r := l.rec()
	if r.set == nil {
		r.set = map[string]string{}
	}
	if _, ok := r.set[name]; !ok {
		r.order = append(r.order, name)
	}
	r.set[name] = v
	if r.hidden[name] {
		delete(r.hidden, name)
	}
}

// FieldNames lists all non-internal fields of the line.
func (l *Line) FieldNames() []string {
	l.Field("_") // force automatic extraction
	r := l.Rec
	names := make([]string, 0, len(r.set)+len(r.auto))
	for k := range r.auto {
		if _, dup := r.set[k]; !dup && r.visible(k) {
			names = append(names, k)
		}
	}
	for k := range r.set {
		names = append(names, k)
	}
	return names
}

var reKV = regexp.MustCompile(`(?:^|[\s,;\[(])([A-Za-z_][\w.\-]*)=("(?:[^"\\]|\\.)*"|[^\s,;"\])]*)`)

// autoFields extracts JSON fields (also from a JSON object that ends the line, as
// in "2026-01-02 INFO {...}") or key=value pairs.
func autoFields(text string) map[string]string {
	m := map[string]string{}
	t := strings.TrimSpace(text)
	if strings.HasSuffix(t, "}") {
		if i := strings.IndexByte(t, '{'); i >= 0 {
			dec := json.NewDecoder(strings.NewReader(t[i:]))
			dec.UseNumber()
			var v map[string]any
			if dec.Decode(&v) == nil {
				flattenJSON("", v, m)
				return m
			}
		}
	}
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
	return m
}

func flattenJSON(prefix string, v map[string]any, m map[string]string) {
	for k, x := range v {
		name := prefix + k
		switch x := x.(type) {
		case map[string]any:
			flattenJSON(name+".", x, m)
		case string:
			m[name] = x
		case json.Number:
			m[name] = x.String()
		case bool:
			m[name] = strconv.FormatBool(x)
		case nil:
		default:
			b, _ := json.Marshal(x)
			m[name] = string(b)
		}
	}
}

// ---------------- numbers ----------------

func parseNum(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

func formatNum(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return ""
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	s := strconv.FormatFloat(f, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// ---------------- tables ----------------

const maxColWidth = 60

// makeTable renders rows as aligned text lines that carry their fields, so later
// commands can still filter or sort them by field.
func makeTable(cols []string, rows [][]string, emit Emit) {
	h := &tableHeader{cols: cols, align: make([]bool, len(cols))}
	widths := make([]int, len(cols))
	shown := rows // display values: _time columns are formatted as dates
	copied := false
	for i, c := range cols {
		if c != "_time" {
			continue
		}
		if !copied {
			copied = true
			shown = make([][]string, len(rows))
			for j, r := range rows {
				shown[j] = append([]string(nil), r...)
			}
		}
		for _, r := range shown {
			if r[i] != "" {
				r[i] = formatTime(r[i])
			}
		}
	}
	for i, c := range cols {
		widths[i] = min(strWidth([]rune(c)), maxColWidth)
		numeric, any := c != "_time", false
		for _, r := range shown {
			if r[i] == "" {
				continue
			}
			any = true
			if _, ok := parseNum(r[i]); !ok {
				numeric = false
			}
			widths[i] = min(max(widths[i], strWidth([]rune(r[i]))), maxColWidth)
		}
		h.align[i] = numeric && any
	}
	render := func(vals []string) string {
		var b strings.Builder
		for i, v := range vals {
			if i > 0 {
				b.WriteString("  ")
			}
			v = fitWidth(v, widths[i])
			pad := strings.Repeat(" ", widths[i]-strWidth([]rune(v)))
			if h.align[i] {
				b.WriteString(pad + v)
			} else if i < len(vals)-1 {
				b.WriteString(v + pad)
			} else {
				b.WriteString(v)
			}
		}
		return b.String()
	}
	h.text = render(cols)
	rule := make([]string, len(cols))
	for i := range cols {
		rule[i] = strings.Repeat("-", widths[i])
	}
	h.rule = render(rule)
	for j, r := range rows {
		set := make(map[string]string, len(cols))
		for i, c := range cols {
			set[c] = r[i]
		}
		if !emit(Line{Text: render(shown[j]), Rec: &Record{set: set, row: true, header: h}}) {
			return
		}
	}
}

// fitWidth truncates s to w columns, marking the cut with "…".
func fitWidth(s string, w int) string {
	if strWidth([]rune(s)) <= w {
		return s
	}
	return truncateWidth(s, w-1) + "…"
}

// sortedKeys returns the keys of a set in a stable order (numeric when possible).
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return lessValue(out[i], out[j]) })
	return out
}

func lessValue(a, b string) bool {
	return compareValues(a, b) < 0
}

// compareValues compares numerically when both values are numbers.
func compareValues(a, b string) int {
	x, okx := parseNum(a)
	y, oky := parseNum(b)
	if okx && oky {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}

func errAt(pos int, format string, a ...any) error {
	return &posError{pos: pos, msg: fmt.Sprintf(format, a...)}
}

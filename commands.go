package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// buildSpec parses one command into a Spec and its file operands.
func buildSpec(cmd rawCmd) (*Spec, []word, error) {
	name := cmd.args[0].s
	args := cmd.args[1:]
	switch name {
	case "grep", "egrep", "fgrep":
		return parseGrep(name, args)
	case "sort":
		return parseSort(args)
	case "uniq":
		return parseUniq(args)
	case "wc":
		return parseWc(args)
	case "head":
		return parseHeadTail("head", args)
	case "tail":
		return parseHeadTail("tail", args)
	case "cat":
		return parseCat(args)
	case "cut":
		return parseCut(args)
	case "tr":
		return parseTr(args)
	case "sed":
		return parseSed(args)
	}
	return nil, nil, fmt.Errorf("%s: command not found (see help)", name)
}

func parseGrep(name string, args []word) (*Spec, []word, error) {
	p, err := getopt(name, args, "ivcnolLhHFwxEGrRsaI", "e", map[string]string{
		"ignore-case": "i", "invert-match": "v", "count": "c", "line-number": "n",
		"only-matching": "o", "files-with-matches": "l", "files-without-match": "L",
		"no-filename": "h", "with-filename": "H", "fixed-strings": "F", "word-regexp": "w",
		"line-regexp": "x", "extended-regexp": "E", "basic-regexp": "G", "regexp": "e",
		"recursive": "r", "text": "a",
	})
	if err != nil {
		return nil, nil, err
	}
	pats := p.vals['e']
	ops := p.ops
	if len(pats) == 0 {
		if len(ops) == 0 {
			return nil, nil, fmt.Errorf("usage: grep [options] pattern [file...]")
		}
		pats = []string{ops[0].s}
		ops = ops[1:]
	}
	re, err := compilePattern(pats, p.has('F') || name == "fgrep", p.has('E') || name == "egrep",
		p.has('w'), p.has('x'), p.has('i'))
	if err != nil {
		return nil, nil, fmt.Errorf("grep: invalid regex: %v", err)
	}
	c := &grepCfg{re: re, invert: p.has('v'), count: p.has('c'), number: p.has('n'),
		only: p.has('o'), list: p.has('l'), listNot: p.has('L'),
		forceName: p.has('H'), noName: p.has('h')}
	s := &Spec{Name: "grep", Parallel: true, New: func() Stage { return &grepStage{c: c} }}
	s.Setup = func(pc planCtx, s *Spec) {
		c.perFile = pc.first
		c.withName = pc.first && (pc.multi || c.forceName) && !c.noName
		// per-file state (counts, line numbers) only works in worker threads when
		// every line of a file reaches the same instance.
		s.Parallel = pc.first || !(c.count || c.list || c.listNot || c.number)
	}
	return s, ops, nil
}

func parseSort(args []word) (*Spec, []word, error) {
	p, err := getopt("sort", args, "rnufbsdg", "kt", map[string]string{
		"reverse": "r", "numeric-sort": "n", "unique": "u", "ignore-case": "f",
		"stable": "s", "key": "k", "field-separator": "t",
	})
	if err != nil {
		return nil, nil, err
	}
	c := &sortCfg{reverse: p.has('r'), numeric: p.has('n') || p.has('g'), unique: p.has('u'),
		fold: p.has('f'), stable: p.has('s')}
	if t, ok := p.last('t'); ok {
		c.sep = t
	}
	if k, ok := p.last('k'); ok {
		a, b, _ := strings.Cut(k, ",")
		parseKey := func(s string) (int, error) {
			s = strings.TrimRightFunc(s, func(r rune) bool {
				switch r {
				case 'n', 'g':
					c.numeric = true
				case 'r':
					c.reverse = true
				case 'f':
					c.fold = true
				case 'b':
				default:
					return false
				}
				return true
			})
			if i := strings.IndexByte(s, '.'); i >= 0 {
				s = s[:i]
			}
			if s == "" {
				return 0, nil
			}
			return strconv.Atoi(s)
		}
		if c.k1, err = parseKey(a); err != nil || c.k1 < 1 {
			return nil, nil, fmt.Errorf("sort: invalid key %q", k)
		}
		if b != "" {
			if c.k2, err = parseKey(b); err != nil {
				return nil, nil, fmt.Errorf("sort: invalid key %q", k)
			}
		}
	}
	return &Spec{Name: "sort", Blocking: true, New: func() Stage { return &sortStage{c: c} }}, p.ops, nil
}

func parseUniq(args []word) (*Spec, []word, error) {
	p, err := getopt("uniq", args, "cdui", "", map[string]string{
		"count": "c", "repeated": "d", "unique": "u", "ignore-case": "i",
	})
	if err != nil {
		return nil, nil, err
	}
	c := &uniqCfg{count: p.has('c'), dups: p.has('d'), uniques: p.has('u'), fold: p.has('i')}
	return &Spec{Name: "uniq", New: func() Stage { return &uniqStage{c: c} }}, p.ops, nil
}

func parseWc(args []word) (*Spec, []word, error) {
	p, err := getopt("wc", args, "lwcm", "", map[string]string{
		"lines": "l", "words": "w", "bytes": "c", "chars": "m",
	})
	if err != nil {
		return nil, nil, err
	}
	c := &wcCfg{lines: p.has('l'), words: p.has('w'), bytes: p.has('c'), chars: p.has('m')}
	if !c.lines && !c.words && !c.bytes && !c.chars {
		c.lines, c.words, c.bytes = true, true, true
	}
	s := &Spec{Name: "wc", Blocking: true, New: func() Stage { return &wcStage{c: c} }}
	s.Setup = func(pc planCtx, _ *Spec) { c.perFile = pc.first }
	return s, p.ops, nil
}

var reDashNum = regexp.MustCompile(`^-[0-9]+$`)

func parseHeadTail(name string, args []word) (*Spec, []word, error) {
	norm := make([]word, 0, len(args)+1)
	for _, a := range args {
		if reDashNum.MatchString(a.s) {
			norm = append(norm, word{s: "-n"}, word{s: a.s[1:]})
		} else {
			norm = append(norm, a)
		}
	}
	p, err := getopt(name, norm, "", "n", map[string]string{"lines": "n"})
	if err != nil {
		return nil, nil, err
	}
	nStr, _ := p.last('n')
	if nStr == "" {
		nStr = "10"
	}
	from := strings.HasPrefix(nStr, "+")
	n, err := strconv.Atoi(strings.TrimPrefix(nStr, "+"))
	if err != nil || n < 0 {
		return nil, nil, fmt.Errorf("%s: invalid line count: %q", name, nStr)
	}
	if name == "head" {
		return &Spec{Name: "head", New: func() Stage { return &headStage{n: n} }}, p.ops, nil
	}
	if from {
		return &Spec{Name: "tail", New: func() Stage { return &tailFromStage{from: n} }}, p.ops, nil
	}
	return &Spec{Name: "tail", Blocking: true, New: func() Stage { return &tailStage{n: n} }}, p.ops, nil
}

func parseCat(args []word) (*Spec, []word, error) {
	p, err := getopt("cat", args, "n", "", map[string]string{"number": "n"})
	if err != nil {
		return nil, nil, err
	}
	if p.has('n') {
		return &Spec{Name: "cat", New: func() Stage { return &catNumStage{} }}, p.ops, nil
	}
	return &Spec{Name: "cat", Parallel: true, New: func() Stage { return passStage{} }}, p.ops, nil
}

func parseCut(args []word) (*Spec, []word, error) {
	p, err := getopt("cut", args, "s", "dfcb", map[string]string{
		"delimiter": "d", "fields": "f", "characters": "c", "bytes": "b", "only-delimited": "s",
	})
	if err != nil {
		return nil, nil, err
	}
	c := &cutCfg{delim: "\t", onlyD: p.has('s')}
	if d, ok := p.last('d'); ok {
		if d == "" {
			return nil, nil, fmt.Errorf("cut: empty delimiter")
		}
		c.delim = d
	}
	list, ok := p.last('f')
	if !ok {
		if list, ok = p.last('c'); !ok {
			list, ok = p.last('b')
		}
		c.byChar = ok
	}
	if !ok {
		return nil, nil, fmt.Errorf("usage: cut -f list [-d delim] | cut -c list")
	}
	if c.ranges, err = parseRanges(list); err != nil {
		return nil, nil, fmt.Errorf("cut: %v", err)
	}
	return &Spec{Name: "cut", Parallel: true, New: func() Stage { return cutStage{c: c} }}, p.ops, nil
}

func parseTr(args []word) (*Spec, []word, error) {
	p, err := getopt("tr", args, "ds", "", map[string]string{"delete": "d", "squeeze-repeats": "s"})
	if err != nil {
		return nil, nil, err
	}
	c := &trCfg{del: p.has('d'), squeeze: p.has('s'), inSet1: map[rune]bool{},
		mapping: map[rune]rune{}, squeezeSet: map[rune]bool{}}
	if len(p.ops) < 1 || len(p.ops) > 2 {
		return nil, nil, fmt.Errorf("usage: tr [-ds] set1 [set2]")
	}
	c.set1 = expandTrSet(p.ops[0].s)
	if len(p.ops) == 2 {
		c.set2 = expandTrSet(p.ops[1].s)
	}
	for _, r := range c.set1 {
		c.inSet1[r] = true
	}
	if !c.del && len(c.set2) > 0 {
		for i, r := range c.set1 {
			j := min(i, len(c.set2)-1)
			c.mapping[r] = c.set2[j]
		}
	} else if !c.del && !c.squeeze {
		return nil, nil, fmt.Errorf("tr: missing set2")
	}
	sq := c.set1
	if len(c.set2) > 0 {
		sq = c.set2
	}
	for _, r := range sq {
		c.squeezeSet[r] = true
	}
	return &Spec{Name: "tr", Parallel: true, New: func() Stage { return trStage{c: c} }}, nil, nil
}

func parseSed(args []word) (*Spec, []word, error) {
	p, err := getopt("sed", args, "nEr", "e", map[string]string{
		"quiet": "n", "silent": "n", "regexp-extended": "E", "expression": "e",
	})
	if err != nil {
		return nil, nil, err
	}
	scripts := p.vals['e']
	ops := p.ops
	if len(scripts) == 0 {
		if len(ops) == 0 {
			return nil, nil, fmt.Errorf("usage: sed [-nE] script [file...]")
		}
		scripts = []string{ops[0].s}
		ops = ops[1:]
	}
	cmds, err := parseSedScript(strings.Join(scripts, "\n"), p.has('E') || p.has('r'))
	if err != nil {
		return nil, nil, err
	}
	c := &sedCfg{quiet: p.has('n'), cmds: cmds}
	return &Spec{Name: "sed", Parallel: true, New: func() Stage { return sedStage{c: c} }}, ops, nil
}

// ---------------- find (names source) ----------------

type findCfg struct {
	roots []string
	names []string // glob on base name
	fold  bool
	typ   byte // 0, 'f' or 'd'
}

func (f *findCfg) match(base string, isDir bool) bool {
	if f.typ == 'f' && isDir || f.typ == 'd' && !isDir {
		return false
	}
	if len(f.names) == 0 {
		return true
	}
	if f.fold {
		base = strings.ToLower(base)
	}
	return matchAny(f.names, base)
}

func parseFind(args []word) (*findCfg, error) {
	f := &findCfg{}
	i := 0
	for ; i < len(args) && !strings.HasPrefix(args[i].s, "-"); i++ {
		f.roots = append(f.roots, args[i].s)
	}
	for ; i < len(args); i++ {
		a := args[i].s
		if i+1 >= len(args) {
			return nil, fmt.Errorf("find: %s requires a value", a)
		}
		v := args[i+1].s
		i++
		switch a {
		case "-name":
			f.names = append(f.names, v)
		case "-iname":
			f.names = append(f.names, strings.ToLower(v))
			f.fold = true
		case "-type":
			if v != "f" && v != "d" {
				return nil, fmt.Errorf("find: -type supports only f or d")
			}
			f.typ = v[0]
		default:
			return nil, fmt.Errorf("find: unsupported predicate %s (-name, -iname, -type)", a)
		}
	}
	if len(f.roots) == 0 {
		f.roots = []string{"."}
	}
	return f, nil
}

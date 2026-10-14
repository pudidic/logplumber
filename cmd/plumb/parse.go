package main

import (
	"errors"
	"fmt"
	"strings"
)

// token is one shell word or operator ("|", ">", ">>", ";").
type token struct {
	s      string
	quoted bool // any part of the word was quoted/escaped (disables glob expansion)
	op     string
	pos    int  // rune offset of the token in the input line
	raw    bool // unparsed argument text of an expression command
}

type word struct {
	s        string
	quoted   bool
	raw      bool // whole argument text of an expression command (where, stats, ...)
	pos      int  // rune offset in the input line
	nameOnly bool // a file name to look for in all subdirectories (SPL source=)
}

type rawCmd struct{ args []word }

type pipelineAST struct {
	cmds     []rawCmd
	redirect string
	appendTo bool
	spl      bool // an SPL query (see spl.go)
}

// Backslash only escapes shell-special characters, so Windows paths such as
// dir\sub\file.txt and regexes such as \d work unquoted.
func isEscapable(c rune) bool { return strings.ContainsRune(" \t'\"|><;\\*?[#", c) }

func lex(line string) ([]token, error) {
	toks, _, err := lexPos(line)
	if err != nil {
		return nil, err
	}
	return toks, nil
}

// lexPos tokenizes line. On a lexical error it also returns the tokens before the
// error and the rune offset where the bad token starts.
func lexPos(line string) ([]token, int, error) {
	var toks []token
	var cur strings.Builder
	inWord, quoted, start := false, false, 0
	begin := func(i int) {
		if !inWord {
			inWord, start = true, i
		}
	}
	flush := func() {
		if inWord {
			toks = append(toks, token{s: cur.String(), quoted: quoted, pos: start})
			cur.Reset()
			inWord, quoted = false, false
		}
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			flush()
		case c == '|' || c == ';':
			flush()
			toks = append(toks, token{op: string(c), pos: i})
		case c == '>':
			flush()
			if i+1 < len(rs) && rs[i+1] == '>' {
				toks = append(toks, token{op: ">>", pos: i})
				i++
			} else {
				toks = append(toks, token{op: ">", pos: i})
			}
		case c == '#' && !inWord:
			flush()
			return toks, -1, nil
		case c == '\'':
			begin(i)
			quoted = true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				cur.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return toks, start, errors.New("unterminated single quote")
			}
			i = j
		case c == '"':
			begin(i)
			quoted = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) && (rs[j+1] == '"' || rs[j+1] == '\\') {
					j++
				}
				cur.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return toks, start, errors.New("unterminated double quote")
			}
			i = j
		case c == '\\' && i+1 < len(rs) && isEscapable(rs[i+1]):
			begin(i)
			quoted = true
			cur.WriteRune(rs[i+1])
			i++
		default:
			begin(i)
			cur.WriteRune(c)
		}
	}
	flush()
	return toks, -1, nil
}

// splitStatements splits a token list on ";".
func splitStatements(toks []token) [][]token {
	var out [][]token
	var cur []token
	for _, t := range toks {
		if t.op == ";" {
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func parsePipeline(toks []token) (*pipelineAST, error) {
	ast := &pipelineAST{}
	var cur rawCmd
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.op {
		case "|":
			if len(cur.args) == 0 {
				return nil, errors.New("syntax error: missing command before '|'")
			}
			ast.cmds = append(ast.cmds, cur)
			cur = rawCmd{}
		case ">", ">>":
			if i+1 >= len(toks) || toks[i+1].op != "" {
				return nil, fmt.Errorf("syntax error: file name expected after '%s'", t.op)
			}
			if i+2 != len(toks) {
				return nil, errors.New("syntax error: redirection must be at the end of the line")
			}
			ast.redirect = toks[i+1].s
			ast.appendTo = t.op == ">>"
			i++
		default:
			cur.args = append(cur.args, word{s: t.s, quoted: t.quoted, raw: t.raw, pos: t.pos})
		}
	}
	if len(cur.args) == 0 {
		if len(ast.cmds) > 0 {
			return nil, errors.New("syntax error: missing command after '|'")
		}
		return nil, errors.New("empty command")
	}
	ast.cmds = append(ast.cmds, cur)
	return ast, nil
}

// ---- getopt-style option parsing ----

type parsedOpts struct {
	bools map[rune]bool
	vals  map[rune][]string
	ops   []word
}

func (p *parsedOpts) has(c rune) bool { return p.bools[c] }

func (p *parsedOpts) last(c rune) (string, bool) {
	v := p.vals[c]
	if len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

func getopt(cmd string, args []word, boolFlags, valFlags string, long map[string]string) (*parsedOpts, error) {
	p := &parsedOpts{bools: map[rune]bool{}, vals: map[rune][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i].s
		if a == "--" {
			p.ops = append(p.ops, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			name, val, hasVal := strings.Cut(a[2:], "=")
			short, ok := long[name]
			if !ok {
				return nil, fmt.Errorf("%s: unknown option --%s", cmd, name)
			}
			c := []rune(short)[0]
			if strings.ContainsRune(valFlags, c) {
				if !hasVal {
					if i+1 >= len(args) {
						return nil, fmt.Errorf("%s: option --%s requires a value", cmd, name)
					}
					i++
					val = args[i].s
				}
				p.vals[c] = append(p.vals[c], val)
			} else {
				p.bools[c] = true
			}
			continue
		}
		if len(a) > 1 && a[0] == '-' {
			rs := []rune(a[1:])
			for j := 0; j < len(rs); j++ {
				c := rs[j]
				if strings.ContainsRune(valFlags, c) {
					val := string(rs[j+1:])
					if val == "" {
						if i+1 >= len(args) {
							return nil, fmt.Errorf("%s: option -%c requires a value", cmd, c)
						}
						i++
						val = args[i].s
					}
					p.vals[c] = append(p.vals[c], val)
					break
				}
				if strings.ContainsRune(boolFlags, c) {
					p.bools[c] = true
					continue
				}
				return nil, fmt.Errorf("%s: invalid option -- '%c'", cmd, c)
			}
			continue
		}
		p.ops = append(p.ops, args[i])
	}
	return p, nil
}

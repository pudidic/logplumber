// plumb is a cross-platform, multi-threaded shell for searching and aggregating the
// files below the current directory (including the contents of tar/zip/gz archives)
// with POSIX-style pipelines. Results and progress update in real time, and in an
// interactive terminal the command line is re-run on every keystroke.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const version = "0.2.0"

var errExit = errors.New("exit")

type Shell struct {
	ui      *UI
	threads int
	mu      sync.Mutex
	cancel  context.CancelFunc
	lastSig time.Time
	jobs    *jobCache
	store   *store   // temporary on-disk index (interactive sessions only)
	lang    langMode // SPL or shell syntax

	indexAsked, indexDelete bool // the answer to "delete the index?" on exit
}

// langMode is the query language: SPL (Splunk's search language, the default)
// or POSIX shell pipelines.
type langMode int

const (
	langSPL langMode = iota
	langShell
)

func (m langMode) String() string {
	if m == langShell {
		return "shell"
	}
	return "SPL"
}

func main() {
	initConsole()
	cmd := flag.String("c", "", "run one command line and exit")
	jobs := flag.Int("j", runtime.NumCPU(), "number of worker threads")
	noLive := flag.Bool("nolive", false, "disable type-ahead results (plain line input)")
	noIndex := flag.Bool("noindex", false, "don't build a temporary index on disk")
	shellMode := flag.Bool("sh", false, "start in shell mode (POSIX pipelines) instead of SPL")
	flag.Bool("spl", true, "start in SPL mode (the default)")
	color := flag.String("color", "auto", "file column and colors: auto (in a terminal), always (also in pipes and files) or never")
	flag.Parse()
	mode, err := parseColorMode(*color)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plumb:", err)
		os.Exit(2)
	}
	if os.Getenv("NO_COLOR") != "" && *color == "auto" {
		mode = colorNever
	}
	colorMode = mode

	sh := &Shell{ui: newUI(), threads: max(1, *jobs)}
	if err := loadConfig(); err != nil {
		fmt.Fprintln(os.Stderr, "plumb: config:", err)
	}
	if *shellMode {
		sh.lang = langShell
	}
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	go func() {
		for range sigc {
			sh.mu.Lock()
			sh.lastSig = time.Now()
			if sh.cancel != nil {
				sh.cancel()
			}
			sh.mu.Unlock()
		}
	}()

	if *cmd == "" && flag.NArg() > 0 {
		*cmd = strings.Join(flag.Args(), " ")
	}
	if *cmd != "" {
		if err := sh.runLine(*cmd); err != nil && err != errExit {
			fmt.Fprintln(os.Stderr, "plumb:", err)
			os.Exit(1)
		}
		return
	}

	interactive := isTerminal(os.Stdin)
	if interactive {
		fmt.Printf("plumb %s — Log Plumber (%d threads, %s mode). Type help for commands. Press Esc to exit.\n", version, sh.threads, sh.lang)
	}
	if interactive && !*noIndex {
		st, note, err := openIndex()
		if note != "" {
			fmt.Fprintln(os.Stderr, "plumb:", note)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "plumb: no index:", err)
		} else {
			sh.store = st
			defer func() {
				del := false
				switch {
				case !st.persistent:
				case sh.indexAsked: // type-ahead mode asked already
					del = sh.indexDelete
				default:
					del = askDeleteIndex(st)
				}
				if del {
					if err := st.remove(); err != nil {
						fmt.Fprintln(os.Stderr, "plumb: could not delete the index:", err)
					}
					return
				}
				if err := st.close(); err != nil {
					fmt.Fprintln(os.Stderr, "plumb: could not save the index:", err)
				}
			}()
			st.setRoot(".")
		}
	}
	if interactive && sh.ui.active && !*noLive {
		var in io.Reader = os.Stdin
		if keys := os.Getenv("PLUMB_TEST_KEYS"); keys != "" {
			in = newScriptedKeys(keys) // for automated terminal tests
		}
		err := sh.liveLoop(in)
		if err == nil {
			return
		}
		fmt.Fprintln(os.Stderr, "plumb: type-ahead mode unavailable:", err)
	}

	in := bufio.NewReader(os.Stdin)
	for {
		if interactive {
			fmt.Print(sh.prompt())
		}
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			if err == io.EOF && interactive && sh.recentSigint() {
				fmt.Println()
				continue
			}
			if interactive {
				fmt.Println()
			}
			return
		}
		if err := sh.runLine(line); err != nil {
			if err == errExit {
				return
			}
			fmt.Fprintln(os.Stderr, "plumb:", err)
		}
	}
}

// On Windows, Ctrl+C in cooked console mode makes the read return EOF.
func (sh *Shell) recentSigint() bool {
	time.Sleep(100 * time.Millisecond)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return time.Since(sh.lastSig) < time.Second
}

func (sh *Shell) cwdLabel() string {
	wd, _ := os.Getwd()
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(wd, home) {
		wd = "~" + wd[len(home):]
	}
	return filepath.ToSlash(wd)
}

// modeName is the prompt label of the current mode.
func (sh *Shell) modeName() string {
	if sh.lang == langShell {
		return "sh"
	}
	return "spl"
}

func (sh *Shell) modeColor() string {
	if sh.lang == langShell {
		return "\x1b[32m"
	}
	return "\x1b[35m"
}

func (sh *Shell) promptPlain() string { return sh.modeName() + " " + sh.cwdLabel() + "> " }

// livePrompt returns a prompt that leaves at least half of a w-column line for input
// (a wrapped input line would break the in-place redraw) and its display width.
func (sh *Shell) livePrompt(w int) (string, int) {
	label := sh.cwdLabel()
	if 4+strWidth([]rune(label))+2 > w/2 {
		label = "…/" + filepath.Base(filepath.FromSlash(label))
	}
	if 4+strWidth([]rune(label))+2 > w/2 {
		return sh.modeColor() + sh.modeName() + "\x1b[0m> ", 5
	}
	return sh.modeColor() + sh.modeName() + "\x1b[0m \x1b[34m" + label + "\x1b[0m> ", 4 + strWidth([]rune(label)) + 2
}

func (sh *Shell) prompt() string {
	if sh.ui.active {
		return sh.modeColor() + sh.modeName() + "\x1b[0m \x1b[34m" + sh.cwdLabel() + "\x1b[0m> "
	}
	return sh.promptPlain()
}

func (sh *Shell) runLine(line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	if sh.lang == langSPL && !isBuiltinLine(line) {
		q, err := expandQuery(line)
		if err != nil {
			return err
		}
		ast, err := splParse(q)
		if err != nil {
			return err
		}
		return sh.runAST(ast)
	}
	toks, err := lex(line)
	if err != nil {
		return err
	}
	for _, stmt := range splitStatements(toks) {
		ast, err := parsePipeline(stmt)
		if err != nil {
			return err
		}
		if len(ast.cmds) == 1 && ast.redirect == "" {
			if handled, err := sh.builtin(ast.cmds[0].args); handled {
				if err != nil {
					return err
				}
				continue
			}
		}
		if err := sh.runAST(ast); err != nil {
			return err
		}
	}
	return nil
}

// runAST runs one pipeline in the foreground; Ctrl+C cancels it.
func (sh *Shell) runAST(ast *pipelineAST) error {
	ctx, cancel := context.WithCancel(context.Background())
	sh.mu.Lock()
	sh.cancel = cancel
	sh.mu.Unlock()
	err := sh.runPipeline(ctx, ast, sh.ui, false)
	sh.mu.Lock()
	sh.cancel = nil
	sh.mu.Unlock()
	cancel()
	return err
}

func (sh *Shell) builtin(args []word) (bool, error) {
	switch args[0].s {
	case "exit", "quit":
		return true, errExit
	case "spl", "sh":
		if len(args) > 1 {
			return true, fmt.Errorf("%s: switches the query language and takes no arguments", args[0].s)
		}
		sh.lang = langSPL
		if args[0].s == "sh" {
			sh.lang = langShell
		}
		sh.dropJobCache()
		fmt.Printf("%s mode\n", sh.lang)
		return true, nil
	case "cd":
		dir := ""
		if len(args) > 1 {
			dir = args[1].s
		} else {
			dir, _ = os.UserHomeDir()
		}
		if strings.HasPrefix(dir, "~") {
			home, _ := os.UserHomeDir()
			dir = home + dir[1:]
		}
		if err := os.Chdir(dir); err != nil {
			return true, fmt.Errorf("cd: %v", unwrapPathErr(err))
		}
		if sh.store != nil {
			sh.store.setRoot(".")
		}
		if err := loadConfig(); err != nil { // .plumb.conf of the new directory
			fmt.Fprintln(os.Stderr, "plumb: config:", err)
		}
		return true, nil
	case "config":
		if len(args) > 1 && args[1].s == "reload" {
			if err := loadConfig(); err != nil {
				return true, fmt.Errorf("config: %v", err)
			}
		} else if len(args) > 1 {
			return true, fmt.Errorf("config: unknown argument %q (config or config reload)", args[1].s)
		}
		fmt.Print(activeConfig().describe())
		return true, nil
	case "pwd":
		wd, err := os.Getwd()
		if err != nil {
			return true, err
		}
		fmt.Println(wd)
		return true, nil
	case "threads", "jobs":
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1].s)
			if err != nil || n < 1 {
				return true, fmt.Errorf("threads: expected a positive integer")
			}
			sh.threads = n
		}
		fmt.Printf("worker threads: %d (%d CPUs)\n", sh.threads, runtime.NumCPU())
		return true, nil
	case "clear", "cls":
		if sh.ui.active {
			fmt.Print("\x1b[2J\x1b[H")
		}
		return true, nil
	case "help", "?":
		os.Stdout.WriteString(helpText)
		return true, nil
	case "version":
		fmt.Printf("plumb %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return true, nil
	case "index":
		if sh.store == nil {
			fmt.Println("index: off")
			return true, nil
		}
		st := sh.store.status.Load()
		state := "ready"
		if st.building {
			state = fmt.Sprintf("building %d%%", 100*st.done/max(1, st.total))
		}
		if len(args) > 1 && args[1].s == "clear" {
			wd, _ := os.Getwd()
			if err := sh.store.clear(wd); err != nil {
				return true, fmt.Errorf("index clear: %v", err)
			}
			fmt.Println("index cleared; indexing the current directory again")
			return true, nil
		} else if len(args) > 1 {
			return true, fmt.Errorf("index: unknown argument %q (index or index clear)", args[1].s)
		}
		kind := "kept across sessions"
		if !sh.store.persistent {
			kind = "temporary"
		}
		fmt.Printf("index: %s (%s)\n  directory: %s\n  root:      %s\n  files:     %s\n  disk use:  %s\n",
			state, kind, sh.store.dir, st.root, commas(int64(st.files)), human(st.disk))
		return true, nil
	}
	return false, nil
}

// scriptedKeys replays keystrokes from PLUMB_TEST_KEYS (Go string escapes allowed);
// each key is delivered 120ms apart and a NUL character pauses for 1.5s.
type scriptedKeys struct{ keys []rune }

func newScriptedKeys(s string) *scriptedKeys {
	quoted := strings.ReplaceAll(s, `"`, `\"`) // plain double quotes are typed as-is
	if u, err := strconv.Unquote(`"` + quoted + `"`); err == nil {
		s = u
	}
	return &scriptedKeys{keys: []rune(s)}
}

func (k *scriptedKeys) Read(p []byte) (int, error) {
	if len(k.keys) == 0 {
		time.Sleep(time.Hour)
		return 0, io.EOF
	}
	r := k.keys[0]
	k.keys = k.keys[1:]
	if r == 0 {
		time.Sleep(1500 * time.Millisecond)
		return 0, nil
	}
	time.Sleep(120 * time.Millisecond)
	s := string(r)
	if r == 0x1b && len(k.keys) > 0 && (k.keys[0] == '[' || k.keys[0] == 'O') {
		// deliver an escape sequence (arrow keys) in one read, as a terminal does
		i := 1
		for i < len(k.keys) && !(k.keys[i] >= 0x40 && k.keys[i] <= 0x7e) {
			i++
		}
		i = min(i+1, len(k.keys))
		s += string(k.keys[:i])
		k.keys = k.keys[i:]
	}
	return copy(p, s), nil
}

const helpText = `plumb (Log Plumber) — parallel log search

plumb reads every file below the current directory, including members of
archives (tar, tar.gz/tgz, tar.bz2, zip/jar, gz, bz2 — nested too), in
parallel. It has two query languages; Tab (or the built-ins spl and sh)
switches between them. The prompt shows the mode: spl> or sh>.

SPL mode (default) — Splunk's Search Processing Language
  The first part of a query is a search; commands follow after '|'.
    error status>=500 | stats count by req.path | sort -count
  Search:  error  "db timeout"  fail*  field=value  field!=v  field>=n  field=*
           level IN (warn, error)  NOT x  x OR y  (x y)   (OR binds tighter than AND)
           earliest=-24h latest=now   source=*.jsonl (files to read)   index= is ignored
           earliest=rt (follow new events)   earliest=rt-5m latest=rt (sliding window)
  Commands:
    search <terms>             where <expr>          eval name = <expr>, ...
    rex [field=F] "(?<name>re)"                      regex [field=|field!=]"re"
    stats agg[(f)] [as n], ... [by f, ...]           top|rare [N] f... [by f...]
    timechart [span=5m] [limit=N] agg[(f)], ... [by f]   (with a bar chart)
    bin [f] span=5m [as name]  table f, f*           fields [+|-] f, f*
    rename f AS g, ...         dedup [N] f... [consecutive=true] [keepempty=true]
    sort [N] [-|+]f, ...       head N   tail N       reverse
    fieldsummary               outputcsv file
    aggs: count dc sum avg min max median p50..p99 values first last
  Events come newest first, below a one-line timeline of all matching events
  (events per time slot, time left to right). Fields: JSON and key=value are
  extracted automatically (nested JSON keys joined with dots: req.path);
  built-ins are _raw _src _line _time. In where/eval, "..." is a string and '...' a field.
  Functions: if case coalesce isnull isnotnull isnum len lower upper trim substr
             round floor ceil abs tonumber tostring match like replace min max
             now time relative_time(t, "-1d@d") strftime(t, "%F %T") strptime(s, fmt)
  Time: _time is the first timestamp of a line (ISO 8601, Apache/nginx
  [14/Oct/2026:09:00:00 +0900], syslog "Oct 14 09:00:46", ctime, or an epoch
  time/ts/timestamp field); zoneless times are local. Time values: now, -15m,
  -24h, -7d@d, @h, @d+9h, @w1, 2026-10-14T09:00:00, epoch seconds.

Shell mode — POSIX pipelines
  grep [-ivcnolLhHFwxE] [-e pattern] pattern [file...]   (BRE unless -E)
  sort [-rnufs] [-k N[,M]] [-t sep]     uniq [-cdui]
  wc   [-lwcm]               per file + total when used as the first command
  head [-n N | -N]           tail [-n N | -n +N]   tail -f [-n N] [file...] (follow)
  cut  -f list [-d delim] [-s] | -c list
  tr   [-ds] set1 [set2]     sed [-nE] 's/re/repl/[gip]' | '/re/d' | '/re/p'
  cat  [-n] [file...]        find [path...] [-name glob] [-iname glob] [-type f|d]
  ls   [-la] [dir]           echo text
  cmd | cmd ...  [> file | >> file]   separate commands with ;
  Without file operands the first command reads every file. A glob without a
  path separator (*.log) matches file names in all subdirectories and archives.

Sourcetypes: fields are extracted by the sourcetype of each line, detected
  automatically: _json, access_combined (clientip method uri uri_path status
  bytes referer useragent ...), apache_error (module log_level pid clientip
  error_code message), nginx_error, syslog (host process pid message) and kv.
  Every event also has sourcetype and source. .plumb.conf (current directory) and
  ~/.config/plumb/plumb.conf add sourcetypes, aliases, calculated fields, macros
  (name in backquotes) and saved searches (savedsearch name); see README.

Built-ins (both modes): spl, sh, config [reload], index [clear], cd, pwd,
  threads [N], clear, help, version, exit

Index: kept across sessions (PLUMB_INDEX_DIR or the user cache directory), so
  only new or changed files are indexed again. On exit plumb asks whether to
  delete it; Enter keeps it. plumb -noindex turns it off.

Real-time: earliest=rt... (SPL) and tail -f (shell) keep reading what is
  written to the files; new files are picked up and truncated files reread.
  Tables are redrawn in place. Ctrl+C stops following and shows the results.

Type-ahead: results are recomputed on every keystroke and shown below the
  prompt. An empty line shows the latest events (newest first). If the last
  part of a query is incomplete or invalid it is underlined in red and only
  the complete commands before it are run. Tab switches SPL/shell, Enter runs
  the line with full output, Up/Down browse results or history, Ctrl+U clears
  the line, Ctrl+C stops a run, Esc exits plumb.

Examples (SPL):
  error | stats count by _src
  source=app/app.jsonl level=error | timechart span=5m count by msg
  "Failed password" | rex "from (?<ip>\S+) port" | top ip
  source=app.jsonl | stats count, avg(ms), p95(ms) by req.path | sort -count
  source=app.jsonl level=error | table _time service msg | outputcsv errors
  earliest=rt-5m latest=rt | timechart span=10s count by sourcetype
Examples (shell):
  grep -i error | wc -l
  cat *.log | cut -d' ' -f1 | sort | uniq -c | sort -rn > top.txt
  tail -f *.log | grep -i error
`

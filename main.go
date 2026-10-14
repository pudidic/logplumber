// psh is a cross-platform, multi-threaded shell for searching and aggregating the
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
}

func main() {
	initConsole()
	cmd := flag.String("c", "", "run one command line and exit")
	jobs := flag.Int("j", runtime.NumCPU(), "number of worker threads")
	noLive := flag.Bool("nolive", false, "disable type-ahead results (plain line input)")
	flag.Parse()

	sh := &Shell{ui: newUI(), threads: max(1, *jobs)}
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
			fmt.Fprintln(os.Stderr, "psh:", err)
			os.Exit(1)
		}
		return
	}

	interactive := isTerminal(os.Stdin)
	if interactive {
		fmt.Printf("psh %s — parallel file shell (%d threads). Type help for commands. Press Esc to exit.\n", version, sh.threads)
	}
	if interactive && sh.ui.active && !*noLive {
		var in io.Reader = os.Stdin
		if keys := os.Getenv("PSH_TEST_KEYS"); keys != "" {
			in = newScriptedKeys(keys) // for automated terminal tests
		}
		err := sh.liveLoop(in)
		if err == nil {
			return
		}
		fmt.Fprintln(os.Stderr, "psh: type-ahead mode unavailable:", err)
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
			fmt.Fprintln(os.Stderr, "psh:", err)
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

func (sh *Shell) promptPlain() string { return "psh " + sh.cwdLabel() + "> " }

// livePrompt returns a prompt that leaves at least half of a w-column line for input
// (a wrapped input line would break the in-place redraw) and its display width.
func (sh *Shell) livePrompt(w int) (string, int) {
	label := sh.cwdLabel()
	if 4+strWidth([]rune(label))+2 > w/2 {
		label = "…/" + filepath.Base(filepath.FromSlash(label))
	}
	if 4+strWidth([]rune(label))+2 > w/2 {
		return "\x1b[32mpsh\x1b[0m> ", 5
	}
	return "\x1b[32mpsh\x1b[0m \x1b[34m" + label + "\x1b[0m> ", 4 + strWidth([]rune(label)) + 2
}

func (sh *Shell) prompt() string {
	if sh.ui.active {
		return "\x1b[32mpsh\x1b[0m \x1b[34m" + sh.cwdLabel() + "\x1b[0m> "
	}
	return sh.promptPlain()
}

func (sh *Shell) runLine(line string) error {
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
		ctx, cancel := context.WithCancel(context.Background())
		sh.mu.Lock()
		sh.cancel = cancel
		sh.mu.Unlock()
		err = sh.runPipeline(ctx, ast, sh.ui, false)
		sh.mu.Lock()
		sh.cancel = nil
		sh.mu.Unlock()
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func (sh *Shell) builtin(args []word) (bool, error) {
	switch args[0].s {
	case "exit", "quit":
		return true, errExit
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
		fmt.Print(helpText)
		return true, nil
	case "version":
		fmt.Printf("psh %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return true, nil
	}
	return false, nil
}

// scriptedKeys replays keystrokes from PSH_TEST_KEYS (Go string escapes allowed);
// each key is delivered 120ms apart and a NUL character pauses for 1.5s.
type scriptedKeys struct{ keys []rune }

func newScriptedKeys(s string) *scriptedKeys {
	if u, err := strconv.Unquote(`"` + s + `"`); err == nil {
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
	return copy(p, string(r)), nil
}

const helpText = `psh — parallel file shell

Input:  When the first command has no file operands, every file in the current
        directory and all subdirectories is read. Archives (tar, tar.gz/tgz,
        tar.bz2, zip/jar, gz, bz2 — nested too) are unpacked on the fly; their
        members are shown as "archive::member/path". Binary files are skipped.
        A glob without a path separator (e.g. *.log) matches file names in all
        subdirectories and inside archives. Quote it to disable globbing.

Pipeline:  cmd | cmd | ...   [> file | >> file]   separate commands with ;

Commands:
  grep [-ivcnolLhHFwxE] [-e pattern] pattern [file...]   (BRE unless -E)
  sort [-rnufs] [-k N[,M]] [-t sep]
  uniq [-cdui]
  wc   [-lwcm]               per file + total when used as the first command
  head [-n N | -N]           tail [-n N | -n +N]
  cut  -f list [-d delim] [-s] | -c list
  tr   [-ds] set1 [set2]     ([:upper:] [:lower:] [:digit:] a-z ranges)
  sed  [-nE] 's/re/repl/[gip]' | '/re/d' | '/re/p'
  cat  [-n] [file...]
  find [path...] [-name glob] [-iname glob] [-type f|d]   lists archive members too
  ls   [-la] [dir]           echo text

Built-ins: cd, pwd, threads [N], clear, help, version, exit

Type-ahead: results are recomputed on every keystroke and shown below the
  prompt. If the last part of a pipeline is incomplete or invalid it is
  underlined in red and only the complete commands before it are run.
  Enter runs the line with full output, Up/Down browse history,
  Ctrl+U clears the line, Ctrl+C stops a run, Esc exits psh.

Live progress: the status line shows percent done, throughput and line counts.
  Commands that need all input (sort, wc, tail) show a live preview while
  reading. Ctrl+C interrupts a running pipeline.

Examples:
  grep -i error | wc -l
  grep -ohE '[A-Za-z_]+' *.go | sort | uniq -c | sort -rn | head
  cat *.log | cut -d' ' -f1 | sort | uniq -c | sort -rn > top.txt
  find -name '*.txt' | wc -l
`

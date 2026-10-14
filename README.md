# psh — parallel file shell

psh runs POSIX-style pipelines over every file in the current directory and its
subdirectories, including files inside archives. It uses multiple threads and
shows progress and results in real time. In an interactive terminal the command
line is **re-run on every keystroke**.

Its only dependencies are the Go standard library and `golang.org/x/term` (for
terminal raw mode).

## Build

```bash
go build -o psh .                                   # current OS
GOOS=linux   GOARCH=amd64 go build -o psh-linux .
GOOS=darwin  GOARCH=arm64 go build -o psh-mac .
GOOS=windows GOARCH=amd64 go build -o psh.exe .
go test ./...
```

## Run

```bash
psh                       # interactive shell (results update as you type)
psh -nolive               # plain line input
psh -c "grep -i error | wc -l"
psh -j 8                  # number of worker threads (default: number of CPUs)
```

Type `help` inside the shell for the list of commands and options.

## Type-ahead mode

| Key | Action |
|---|---|
| Any character | Recompute the results and show them below the prompt |
| **Esc** | **Exit psh** (always shown in the hint bar at the bottom) |
| Enter | Run the line with full output, which stays in the scrollback |
| ↑ / ↓ | Command history |
| ← / →, Home / End, Del | Move the cursor, delete |
| Ctrl+U / Ctrl+K / Ctrl+W | Clear the line, delete to end of line, delete the previous word |
| Ctrl+C | Stop the running preview; otherwise clear the line |

- The pipeline is checked command by command from the left. The first command
  that is incomplete or invalid, and everything after it, is **underlined in
  red**. Only the complete commands before it are run.
  Example: in `grep -i apple | so`, `so` is underlined and the results of
  `grep -i apple` are shown.
- Redirections (`>`), multiple commands (`;`) and built-ins (`cd`, …) are not
  previewed. They run only when you press Enter.
- The file list is cached for 10 seconds, so typing does not rescan the
  directory tree on every key. Pressing Enter clears the cache.

## Layout

| File | Role |
|---|---|
| `main.go` | Entry point, line-input REPL, built-ins, Ctrl+C handling, help text |
| `live.go` | Type-ahead mode: key decoding, line editing, incomplete-command analysis, rendering |
| `parse.go` | Tokenizer that records positions; parsing of `\|` `>` `>>` `;`; getopt |
| `commands.go` | Per-command option parsing into a Spec |
| `stages.go` | grep, sort (parallel), uniq, wc, head, tail, cat, cut, tr, sed |
| `source.go` | File discovery, archive decoding (tar/gz/bz2/zip, nested), worker threads, progress counters |
| `run.go` | Execution engine: parallel/serial split, live previews, status line, file-list cache |
| `ui.go`, `term_*.go` | ANSI live-region rendering, OS-specific terminal handling |

### Execution model

1. psh collects the list of input files first. Their total size is the
   denominator of the progress percentage.
2. The stateless, line-by-line commands at the front of the pipeline (grep, cut,
   tr, sed, cat) run in **N worker threads**, each handling whole files.
3. The order-dependent commands that follow (sort, uniq, wc, head, …) run in a
   single serial goroutine.
4. Stages that need all of their input (sort, wc, tail) are snapshotted
   periodically. Each snapshot is passed through fresh copies of the later
   stages to draw a **live preview**.
5. Once `head` has enough lines, reading stops early.
6. In type-ahead mode every keystroke cancels the running pipeline and starts a
   new one 40 ms later. Each run has a generation number, so late results from
   a cancelled run are discarded.

## Limitations

- Regular expressions use Go's RE2 syntax. Backreferences (`\1`) cannot be used
  in patterns, only in sed replacement strings.
- `tr` works line by line, so it does not translate the newline character.
- xz and zstd archives are not supported (they are not in the standard library).
- Files are read in parallel, so the order of output between files can change
  from run to run. The order of lines within a file is preserved.
- With very many files, each keystroke rereads all of them. After the first
  run, the OS file cache makes this fast.

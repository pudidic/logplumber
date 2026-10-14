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

## Field commands

Besides the POSIX text tools, psh has pipeline commands that work on fields.
JSON lines (also a JSON object at the end of a line) and `key=value` text are
parsed automatically. Nested JSON keys are joined with dots (`req.path`). Every
line also has `_raw`, `_src` (file name) and `_line`.

| Command | Example |
|---|---|
| `rex [field=F] "regex"` | `rex "\" (?<status>\d{3}) (?<bytes>\d+)"` — named groups become fields |
| `where <condition>` | `where status >= 500 and req.path like "/api/%"` |
| `eval name = <expr>, ...` | `eval sec = round(ms / 1000, 1), slow = if(ms > 1000, "y", "n")` |
| `stats agg, ... [by f, ...]` | `stats count, avg(ms), p95(ms) as p95 by host` |
| `top` / `rare [N] field... [by f...]` | `top 10 msg by level` |
| `table field...` | `table _src status user` |
| `fieldsummary` | fields with coverage, distinct count, min/max and top values |
| `sort [N] -field +field` | `stats count by user \| sort -count` — with N only the first N lines are kept in memory |

- Aggregations: `count`, `dc`, `sum`, `avg`, `min`, `max`, `median`, `p50`…`p99`,
  `values`, `first`, `last`. Rows are ordered by the first aggregation, largest first.
- Operators: `= == != < <= > >= + - * / %`, `and or not`, `in (...)`, `like`
  (`%` and `_` wildcards). Functions: `if case coalesce isnull isnotnull isnum len
  lower upper trim substr round floor ceil abs tonumber tostring match like replace min max`.
- A missing field is null. Comparisons with null are false, so `where status != 200`
  skips lines without a status.
- Results of `stats`, `top`, `table` and `fieldsummary` are aligned tables whose rows
  keep their fields, so later commands can filter (`where count > 10`) or sort them.
- `stats` and `top` pre-aggregate in every worker thread and merge the partial
  results, and they update a live preview while reading.
- Expression commands receive their argument text verbatim, so `>` inside
  `where`/`eval` is a comparison. Redirection still works after `stats`, `top`,
  `table` and the text commands. Expression commands take no file operands; use
  `cat file | where ...`.

## Time

Every line gets a `_time` field: the first timestamp near the start of the line,
as Unix seconds. Recognized formats are ISO 8601 / RFC 3339 (`T` or space, `,` or
`.` fractions, `Z` or an offset), `2026/10/14 09:00:00`, Apache/nginx
`[14/Oct/2026:09:00:00 +0900]`, ctime (`Wed Oct 14 09:01:48 2026`) and BSD syslog
(`Oct 14 09:00:46`, the year is the latest one that is not in the future). If the
text has none, a JSON or key=value field named `time`, `ts`, `timestamp` or
`@timestamp` holding epoch seconds or milliseconds is used. Times without a zone
are local time. Tables show `_time` as a date, but the value stays numeric, so
`where`, `sort -_time` and arithmetic work on it.

| Command | Example |
|---|---|
| `search earliest=T [latest=T]` | `search earliest=-24h` — events in `[earliest, latest)`; lines without a time are dropped |
| `bin [field] span=S [as name]` | `bin span=5m \| stats count by _time`, `bin ms span=100` (gives `100-200`) |
| `timechart [span=S] agg, ... [by field] [limit=N]` | `timechart span=1m count by level`, `timechart p95(ms)` |

- Time values: `now`, `-15m`, `-24h`, `-7d`, snaps such as `@h`, `-1d@d`, `@w1`
  (Monday), `@mon`, offsets after a snap (`@d+9h`), absolute times
  (`2026-10-14T09:00:00`, `2026-10-14`, `10/14/2026:09:00:00`) and epoch seconds.
- Spans: `s m h d w mon q y` (`30s`, `5m`, `1h`, `1d`, `1w`, `1mon`); a plain number
  is seconds. Buckets of a day or less start at local midnight; weeks start on Sunday.
- `timechart` gives one row per bucket in time order and fills empty buckets
  (`0` for `count`/`dc`). Without `span` it starts at 1s and switches to coarser
  spans (10s, 30s, 1m, 5m, … 1w) while there are more than 100 buckets. With `by`,
  the `limit` (default 10) largest series are kept and the rest are summed into
  `OTHER` (`limit=0` keeps all). A bar column is added for a single series, or
  stacked bars (`█ ▓ ▒ ░`) for up to four `count`/`sum` series.
- Functions: `now()` (when the query started), `time()`, `relative_time(t, "-1d@d")`,
  `strftime(t, "%Y-%m-%d %H:%M")`, `strptime(s, "%d/%b/%Y:%H:%M:%S")`.

## Type-ahead mode

With an empty command line psh shows the **latest events**: the 1000 newest lines
that have a timestamp, newest first (`where isnotnull(_time) | sort 1000 -_time`).
It appears at start-up and again whenever the line is cleared.

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
| `record.go` | Fields of a line: automatic JSON/key=value extraction, aligned table output |
| `expr.go` | Expression language of `where` and `eval` (lexer, parser, evaluator) |
| `fieldcmds.go` | rex, where, eval, stats (mergeable), top, rare, table, fieldsummary, field sort |
| `timeutil.go` | Timestamp detection, spans and bucketing, relative times, strftime/strptime |
| `timecmds.go` | search (time range), bin, timechart (on the stats engine, bar charts) |
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

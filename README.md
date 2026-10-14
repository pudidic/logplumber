# psh — parallel file shell

psh searches every file in the current directory and its subdirectories,
including files inside archives. It uses multiple threads and shows progress and
results in real time. In an interactive terminal the query is **re-run on every
keystroke**.

It has two query languages, each with its own unchanged syntax:

- **SPL mode** (default, prompt `spl>`): Splunk's Search Processing Language.
  `error status>=500 | stats count by req.path | sort -count`
- **Shell mode** (prompt `psh>`): POSIX pipelines.
  `grep -i error | cut -d' ' -f1 | sort | uniq -c | sort -rn > top.txt`

**Tab** switches between them (or the built-ins `spl` and `sh`, or `psh -sh` at
start-up).

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
psh                       # interactive, SPL mode (results update as you type)
psh -sh                   # start in shell mode
psh -nolive               # plain line input
psh -c 'error | stats count by _src'
psh -sh -c "grep -i error | wc -l"
psh -j 8                  # number of worker threads (default: number of CPUs)
psh -color=always -c 'error' | less -R   # file column and colors in a pipe
```

Type `help` inside the shell for the list of commands and options.

## SPL mode

A query starts with a search; commands follow after `|`. Commands, quoting and
operators follow Splunk: `"..."` is a string, `'...'` is a field name inside
`where`/`eval`, and `>` is always a comparison (there is no redirection; use
`outputcsv`).

### Search

| Syntax | Matches |
|---|---|
| `error` | events containing the word (case-insensitive, whole words: not `errors`) |
| `"db timeout"` | the phrase |
| `fail*` | wildcards |
| `status=500` `status!=200` `ms>1000` `user=k*` | field comparisons; `=` values are case-insensitive and may use `*`; `!=` needs the field |
| `user=*` | events that have the field |
| `level IN (warn, error)` | any of the values |
| `NOT a`, `a OR b`, `a AND b`, `(a b)` | uppercase operators; terms side by side mean AND; as in Splunk, OR binds tighter than AND |
| `earliest=-24h latest=now` | time range `[earliest, latest)` |
| `earliest=rt`, `earliest=rt-5m latest=rt` | real-time: follow the files (see below) |
| `source=*.jsonl` `source=app/app.log` | files to read: a name matches in all subdirectories and archives |
| `index=main` | accepted and ignored (psh has no Splunk indexes) |

`*` alone matches everything: `* | stats count`. A query such as `stats count`
searches for the words "stats" and "count", as in Splunk; type-ahead mode points
this out.

### Commands

| Command | Example |
|---|---|
| `search <terms>` | `\| search status=200` |
| `where <expr>` | `where status >= 500 and like(req.path, "/api/%")` |
| `eval name = <expr>, ...` | `eval sec = round(ms / 1000, 1), slow = if(ms > 1000, "y", "n")` |
| `rex [field=F] "regex"` | `rex "from (?<ip>\S+) port"` — named groups become fields |
| `regex [field=\|field!=]"re"` | `regex user!="^bot"` |
| `stats agg, ... [by f, ...]` | `stats count, avg(ms), p95(ms) as p95 by host` |
| `top` / `rare [N] field... [by f...]` | `top 10 msg by level` |
| `timechart [span=S] [limit=N] agg, ... [by f]` | `timechart span=5m count by level` |
| `bin [f] span=S [as name]` | `bin span=5m \| stats count by _time` |
| `table f, f*` | `table _time user req.*` |
| `fields [+\|-] f, f*` | `fields - req.*` |
| `rename f AS g, ...` | `rename count AS n` |
| `dedup [N] f... [consecutive=true] [keepempty=true]` | `dedup user` |
| `sort [N] [-\|+]f, ...` | `sort -count, user` — with N only the first N rows are kept in memory |
| `head N`, `tail N`, `reverse` | `head 20` |
| `fieldsummary` | fields with coverage, distinct count, min/max and top values |
| `outputcsv file` | `outputcsv errors` writes `errors.csv` and passes the results on |

- Fields are extracted automatically by the line's sourcetype (JSON,
  key=value, access logs, error logs, syslog and your own formats; see below).
  Every event also has `_raw`, `_src` (file name), `_line`, `_time`, `source`
  and `sourcetype`.
- **Timeline:** above the events of a search (not above `stats`, `timechart`
  and other tables), one line shows the events per time slot, with time running
  left to right:
  `09:01 ▆▃▃▅▆  ▂ ▂ ▂▂▅ 11:26 · 5m/col · max 3 · 23 events`. The span per column
  is the finest that fits the terminal width. It counts every matching event,
  also those that `head` or the type-ahead preview leave out. A few stray
  timestamps far from the rest (from 200 events on, at most 0.5% at each end)
  are left out of the range and counted as "outside". It is shown in type-ahead
  mode and above Enter output in a terminal, not when the output goes to a
  file or pipe.
- **Events come newest first**, as in Splunk: a search is sorted by `_time`
  before the first command that depends on order (`head`, `dedup`, `table`, …).
  `head N` right after the search keeps only the newest N in memory. Commands
  that make their own order (`stats`, `sort`, `timechart`, …) skip the sort.
- Aggregations: `count`, `dc`, `sum`, `avg`, `min`, `max`, `median`, `p50`…`p99`,
  `values`, `first`, `last`. `stats` rows are ordered by the first aggregation,
  largest first. Without `by`, `stats` gives one row even for no events (`count 0`).
- Expression operators: `= == != < <= > >= + - * / %`, `and or not`, `in (...)`,
  `like` (`%` and `_` wildcards). Functions: `if case coalesce isnull isnotnull
  isnum len lower upper trim substr round floor ceil abs tonumber tostring match
  like replace min max now time relative_time strftime strptime`.
- A missing field is null. Comparisons with null are false, so `where status != 200`
  skips events without a status.
- Results of `stats`, `top`, `timechart`, `table` and `fieldsummary` are tables
  whose rows keep their fields, so later commands can filter, sort, `eval`,
  `rename` or `fields` them; the table is redrawn with the new columns.
- `stats`, `top` and `timechart` pre-aggregate in every worker thread and merge
  the partial results, and they update a live preview while reading.

## Real-time

psh can keep following the files and show what is written to them, like
`tail -f` and Splunk's real-time searches.

| Query | Shows |
|---|---|
| `earliest=rt level=error` | new matching events as they are written |
| `earliest=rt-5m latest=rt \| stats count by status` | the last 5 minutes first, then a sliding 5-minute window: events older than 5 minutes drop out of the counts |
| `earliest=rt-1m latest=rt \| timechart span=5s count by level` | a chart of the last minute that moves along |
| `tail -f [-n N] [file...]` (shell mode) | the last N lines (default 10) of each file, then new lines: `tail -f app.log \| grep -i error` |

- Events print as they arrive; tables (`stats`, `timechart`, `top`, …) are
  redrawn in place. The status line shows the files followed and the new lines.
- **Ctrl+C stops following** and prints the final results. In type-ahead mode
  the preview follows while you look at it; typing starts a new query.
- psh checks the files four times a second. Files that appear later are read
  from the beginning (also files named on `tail -f` that do not exist yet), and
  a file that becomes shorter (truncated or rotated) is read again from the
  start. Archives are not followed. psh never follows the file its own output
  is redirected to.
- `earliest=rt` and `latest=rt` go at the top level of the first search.

## Sourcetypes and `.psh.conf`

Each line gets a **sourcetype** that decides which fields are extracted. The
built-in ones are recognized from the line itself:

| sourcetype | Lines | Fields |
|---|---|---|
| `_json` | a JSON object (also one that ends the line) | its keys; nested keys joined with dots (`req.path`) |
| `access_combined` | Apache/nginx combined access log | `clientip ident user method uri uri_path uri_query version status bytes referer useragent`, plus `key=value` after it (nginx `rt=`) |
| `apache_error` | Apache 2.4 error log | `module log_level pid clientip clientport error_code message` |
| `nginx_error` | nginx error log | `log_level pid message clientip server request upstream host` |
| `syslog` | BSD syslog | `host process pid message`, plus `key=value` in the message |
| `kv` | `key=value` pairs (logfmt) | the pairs |

Every event also has `sourcetype` and `source` (the file, same as `_src`), so
`sourcetype=access_combined status>=500 | stats count by uri_path` and
`* | stats count by sourcetype` work without `rex`. Lines that match none have no
sourcetype and no automatic fields.

`.psh.conf` in the current directory (re-read after `cd`) and
`~/.config/psh/psh.conf` add to this; the `config` built-in shows what is loaded
and `config reload` re-reads it. The file is INI style; `.psh.conf` itself is not
searched.

```ini
# A new sourcetype, chosen by file name (source=) or line pattern (detect=)
[sourcetype:payments]
source = payments*.log
extract = ^(?<ts>\S+ \S+) (?<level>\w+)\s+\[(?<service>[^\]]+)\] order (?<order>\d+) charged (?<amount>\d+)
time_field = ts                          # _time from this field ...
time_format = %d.%m.%Y %H:%M:%S,%3N      # ... with a strptime format
eval = amount_k = round(amount / 1000, 1)  # calculated fields, in order

# Additions to a built-in sourcetype (alias, eval, extract, time_field/time_format)
[sourcetype:access_combined]
alias = clientip AS src_ip
eval = status_class = substr(status, 1, 1) + "xx"

# Macros: `errors` and `slow(1000)` in a query
[macro:errors]
definition = (level=error OR log_level=error OR status>=500)

[macro:slow(1)]
args = ms
definition = (ms>$ms$ OR duration_ms>$ms$)

# Saved searches: savedsearch failed-logins [| more commands]
[search:failed-logins]
search = sourcetype=syslog "Failed password" | rex "from (?<ip>\S+) port" | top ip
```

| Key (`[sourcetype:…]`) | Meaning |
|---|---|
| `source` | file name glob; matches the path or the name, archive members too (repeatable) |
| `detect` | regex that lines of this sourcetype match |
| `extract` | regex whose named groups become fields (repeatable) |
| `kv`, `json` | `true` to also extract `key=value` pairs, or to parse lines as JSON |
| `time_field`, `time_format` | take `_time` from a field, with a strptime format (`%d %m %Y %H %M %S %3N %b %z` …); without a format the usual detection is applied to the field |
| `alias` | `field AS other` (repeatable) |
| `eval` | `name = expression`, as in `eval` (repeatable) |

New sourcetypes are tried before the built-in ones, in file order. Macro
arguments are written `$name$` in the definition; macros may use other macros.

## Shell mode

POSIX text tools: `grep`, `sort`, `uniq`, `wc`, `head`, `tail` (also `tail -f`),
`cat`, `cut`, `tr`, `sed`, `find`, `ls`, `echo`, with `|`, `;`, `> file` and `>> file`. Without
file operands the first command reads every file. A glob without a path separator
(`*.log`) matches file names in all subdirectories and inside archives. SPL
commands are not available here (and shell commands not in SPL mode); the error
message says to press Tab.

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

| SPL | Example |
|---|---|
| `earliest=T [latest=T]` in a search | `earliest=-24h error` — events in `[earliest, latest)`; events without a time are dropped |
| `bin [field] span=S [as name]` | `* \| bin span=5m \| stats count by _time`, `bin ms span=100` (gives `100-200`) |
| `timechart [span=S] agg, ... [by field] [limit=N]` | `* \| timechart span=1m count by level`, `timechart p95(ms)` |

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

## Index

In an interactive session psh builds an index in the background (the hint bar
shows `index ready · N files`): archive and compressed members are unpacked
once into a cache, and every file is split into chunks of about 256KB. For each
chunk psh records its trigrams (3-byte sequences) in an inverted index, and its
words (runs of ASCII letters, digits and `_`) in a Bloom filter.

The index is **kept across sessions** in the user cache directory
(`~/.cache/psh/index` on Linux, `~/Library/Caches/psh/index` on macOS,
`%LocalAppData%\psh\index` on Windows; `PSH_INDEX_DIR` overrides it). Files
are recorded by absolute path, so every directory you searched stays indexed,
and the next start only indexes new or changed files: on 1GB, the first build
takes about 18s, the next start is ready at once. When psh exits it asks
whether to delete the index; Enter keeps it.

- `index` shows the index state, size and directory; `index clear` deletes it
  and indexes the current directory again.
- Cache files of changed or deleted files are removed at the next start; when
  more than half of the index is stale, it is rebuilt from scratch.
- If another psh is using the index, a temporary one is built and deleted on
  exit, as before.
- `psh -noindex` turns the index off; `psh -c` does not use one.

A query whose first search requires literal text reads only the chunks that may
contain it: the trigram index finds the chunks with all its trigrams, and the
word filters drop those without its whole words. Trigrams alone cannot tell IDs
made of few characters apart (every 3-character piece of a hex trace ID occurs
almost everywhere); the word filters can. Results are the same as without the
index; files not indexed yet, or changed since, are read in full. The summary
line shows the effect: `index: read 4.5MB of 1.0GB`.

Measured on 1GB of mixed logs (4,160 chunks):

| Query | Without index | With index |
|---|---|---|
| a 32-digit hex trace ID | 1.7s | 0.016s (4.5MB read) |
| `INC-77fe31` (22 lines) | 1.7s | 0.013s |
| `"Failed password" \| stats count` (1 in 20 lines) | 1.6s | 0.39s |
| `error \| stats count` (common) | 1.8s | 0.76s |

The index takes about 4% of the data (trigrams 1.5%, word filters 2.7%) and
18s to build for 1GB.

The index is used for words, phrases, wildcard pieces of 3 or more characters,
and `field=value` with a literal text value, combined with AND and OR (in shell
mode: `grep` with a literal pattern as the first command). Words shorter than 3
characters use the word filters alone. It is not used for `NOT`, numeric
comparisons, `*` alone, non-ASCII text, real-time searches, or
`grep -v`/`-c`/`-L`.

## File column

In a terminal, result lines that come from a file are shown in two aligned
columns: the file name (magenta) and the content. This applies to the events of
an SPL search and to grep's `file:` and `file:line:` prefixes (the line number
in green). Names wider than 40 columns are shortened at the front, so the file
name itself stays visible.

`-color` decides where: `auto` (the default) uses columns and colors in a
terminal and plain text (`app/worker.log:11:time=…`) in a pipe or file;
`always` uses them in pipes and files too, e.g.
`psh -color=always -c 'error' | less -R`; `never` turns colors off (also when
`NO_COLOR` is set). A `> file` inside psh always writes plain text.

## Type-ahead mode

With an empty command line psh shows the **latest events**: the 1000 newest lines
that have a timestamp, newest first (the SPL query `_time=*`), in both modes. It
appears at start-up and again whenever the line is cleared.

| Key | Action |
|---|---|
| Any character | Recompute the results and show them below the prompt |
| **Esc** | **Exit psh** (always shown in the hint bar at the bottom) |
| Tab | Switch between SPL and shell mode |
| Enter | Run the line with full output, which stays in the scrollback |
| ↑ / ↓ | Command history |
| ← / →, Home / End, Del | Move the cursor, delete |
| Ctrl+U / Ctrl+K / Ctrl+W | Clear the line, delete to end of line, delete the previous word |
| Ctrl+C | Stop the running preview; otherwise clear the line |

- The query is checked command by command from the left. The first command
  that is incomplete or invalid, and everything after it, is **underlined in
  red**. Only the complete commands before it are run.
  Example: in `apple | so`, `so` is underlined and the results of the search
  `apple` are shown.
- A search preview keeps the newest 1000 events; Enter shows all of them.
- Redirections (`>`), multiple commands (`;`), `outputcsv` and built-ins (`cd`,
  …) are not previewed. They run only when you press Enter.
- The file list is cached for 10 seconds, so typing does not rescan the
  directory tree on every key. Pressing Enter clears the cache.

## Layout

| File | Role |
|---|---|
| `main.go` | Entry point, line-input REPL, modes, built-ins, Ctrl+C handling, help text |
| `live.go` | Type-ahead mode: key decoding, line editing, incomplete-command analysis, rendering |
| `parse.go` | Shell mode: tokenizer that records positions; parsing of `\|` `>` `>>` `;`; getopt |
| `commands.go` | Shell mode: per-command option parsing into a Spec |
| `spl.go` | SPL mode: query splitting, command dispatch, implicit time order, fields/rename/dedup/regex/sort/outputcsv |
| `search.go` | SPL search language (terms, field comparisons, boolean operators, time range, source) |
| `sourcetype.go` | Sourcetypes: built-in detection and parsers (JSON, access, error logs, syslog, kv), custom extraction |
| `config.go` | `.psh.conf`: sourcetypes, aliases, calculated fields, macros, saved searches |
| `stages.go` | grep, sort (parallel), uniq, wc, head, tail, cat, cut, tr, sed |
| `record.go` | Fields of a line (automatic, set, filtered), aligned table output |
| `expr.go` | Expression language of `where` and `eval` (lexer, parser, evaluator) |
| `fieldcmds.go` | rex, where, eval, stats (mergeable), top, rare, table, fieldsummary, field sort |
| `timeutil.go` | Timestamp detection, spans and bucketing, relative times, strftime/strptime |
| `timecmds.go` | bin, timechart (on the stats engine, bar charts) |
| `source.go` | File discovery, archive decoding (tar/gz/bz2/zip, nested), worker threads, progress counters |
| `follow.go` | Real-time: following files (appends, new files, truncation), `tail -f`, sliding window |
| `timeline.go` | One-line event timeline above search results |
| `columns.go` | File column and colors for result lines in a terminal |
| `run.go` | Execution engine: parallel/serial split, live previews, status line, file-list cache |
| `store.go` | On-disk store of an interactive session: archive cache, background indexing |
| `persist.go` | Keeping the index across sessions: manifest, recovery, cleanup, the exit question |
| `index.go`, `bucket.go` | Trigram index: chunks, bucket files, lookup of the chunks a query needs |
| `wordbloom.go` | Per-chunk Bloom filters of words, used to narrow the trigram candidates |
| `browse.go`, `rows.go` | Type-ahead result list and detail view; result storage that spills to disk |
| `ui.go`, `term_*.go` | ANSI live-region rendering, OS-specific terminal handling |

### Execution model

1. psh collects the list of input files first. Their total size is the
   denominator of the progress percentage.
2. The stateless, line-by-line commands at the front of the pipeline (search,
   where, eval, rex, grep, cut, tr, sed, cat, …) run in **N worker threads**,
   each handling whole files.
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
- In shell mode files are read in parallel, so the order of output between files
  can change from run to run. The order of lines within a file is preserved.
- Formats other than the built-in sourcetypes need a `[sourcetype:…]` section
  in `.psh.conf` (or `rex`) for their fields.
- With very many files, each keystroke rereads all of them. After the first
  run, the OS file cache makes this fast.

# Sample logs

Small, synthetic logs in common formats for trying psh. Start psh in this
directory (`cd samples` inside psh, or run it here) and try the queries below.
All addresses use documentation ranges; nothing here is real data.

| Path | Format |
|---|---|
| `apache/access.log` | Apache combined log format |
| `apache/access.log.1.gz` | rotated, gzip-compressed access log |
| `apache/error.log` | Apache 2.4 error log |
| `nginx/access.log` | nginx combined log plus `rt=` and upstream timings |
| `app/app.jsonl` | JSON lines with nested fields (`req.path`, `req.method`) |
| `app/worker.log` | key=value (logfmt) |
| `syslog/messages` | BSD syslog (sshd, kernel, cron, systemd) |
| `archives/support-bundle.tar.gz` | tar.gz bundle with a nested `.gz` log inside |
| `archives/workers.zip` | zip with logs from two nodes |
| `payments/payments.log` | a custom format; its sourcetype is defined in `.psh.conf` |
| `.psh.conf` | sample configuration: the `payments` sourcetype, additions to `access_combined`, macros, saved searches |

## Example queries (SPL mode)

```
# What kinds of lines are there?
* | stats count by sourcetype

# Apache status codes across the current log, the rotated .gz and the bundle
sourcetype=access_combined | stats count, sum(bytes) as bytes by status

# JSON: slowest errors by service and message
level=error | stats count, avg(ms), p95(ms) by service, msg

# Most common paths per level, including the logs inside the tar.gz
source=*.jsonl* | top 3 req.path by level

# logfmt: failed jobs (also from both nodes inside workers.zip)
source=worker.log* result!=ok | stats count, max(duration_ms) by job, result

# syslog: where failed SSH logins come from (also: savedsearch failed-logins)
"Failed password" | rex "from (?<ip>\S+) port" | top ip

# syslog programs per host
sourcetype=syslog | stats count by host, process

# Apache error log: which modules report errors
sourcetype=apache_error log_level=error | top module

# nginx response times
source=nginx/access.log | stats count, avg(rt), p99(rt) by method

# Apache error log modules and severities
sourcetype=apache_error | stats count by module, log_level

# Requests per 5 minutes by level, with a stacked bar chart
source=app/app.jsonl | timechart span=5m count by level

# 5xx errors over time, top 3 paths
source=apache/access.log status>=500 | timechart span=10m count by uri_path limit=3

# The custom payments format (sourcetype from .psh.conf)
sourcetype=payments | stats count, sum(amount) as won, avg(ms) by card, outcome | sort -count
savedsearch declined-payments

# Macros from .psh.conf
`errors` | stats count by sourcetype
source=access.log | stats count by status_class

# Latest 5 errors from anywhere
error | head 5 | table _time _src _raw

# What fields does the JSON log have?
source=app/app.jsonl | fieldsummary
```

## Example queries (shell mode, Tab or `sh`)

```
grep 'Failed password' | grep -o 'from [0-9.]*' | sort | uniq -c | sort -rn
cat nginx/access.log | cut -d' ' -f1 | sort | uniq -c | sort -rn | head
grep -c timeout *.log
find
```

## Regenerating

The files are generated deterministically:

```
go run ./samples/gen samples
```

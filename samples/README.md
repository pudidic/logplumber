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

## Example queries

```
# Apache status codes across the current log, the rotated .gz and the bundle
cat access.log* | rex '"(?<method>\w+) (?<path>\S+) [^"]*" (?<status>\d{3}) (?<bytes>\S+)' | stats count, sum(bytes) as bytes by status

# JSON: slowest errors by service and message
where level = "error" | stats count, avg(ms), p95(ms) by service, msg

# Most common paths per level, including the logs inside the tar.gz
cat *.jsonl* | top 3 req.path by level

# logfmt: failed jobs (also from both nodes inside workers.zip)
cat worker.log* | where result != "ok" | stats count, max(duration_ms) by job, result

# syslog: where failed SSH logins come from
grep 'Failed password' | rex 'from (?<ip>\S+) port' | top ip

# nginx response times
cat nginx/access.log | rex 'rt=(?<rt>[\d.]+)' | stats count, avg(rt), p99(rt)

# Apache error log modules and severities
cat apache/error.log | rex '\[(?<module>\w+):(?<sev>\w+)\]' | stats count by module, sev

# What fields does the JSON log have?
cat app/app.jsonl | fieldsummary

# Requests per minute by level, with a stacked bar chart
cat app/app.jsonl | timechart span=1m count by level

# 5xx errors over time, top 3 paths
cat access.log* | rex '"\w+ (?<path>\S+) [^"]*" (?<status>\d{3})' | where status >= 500 | timechart span=10m count by path limit=3

# Latest 5 events across all logs
sort 5 -_time | table _time _src _raw

# Everything, including archive members
find
```

## Regenerating

The files are generated deterministically:

```
go run ./samples/gen samples
```

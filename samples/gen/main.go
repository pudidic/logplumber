// Command gen writes the deterministic sample logs in samples/.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var rnd = rand.New(rand.NewSource(42))

func pick[T any](xs []T) T { return xs[rnd.Intn(len(xs))] }

var (
	ips    = []string{"203.0.113.7", "198.51.100.23", "192.0.2.41", "203.0.113.88", "198.51.100.5", "192.0.2.199", "10.1.4.12"}
	agents = []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
		"curl/8.9.1", "Googlebot/2.1 (+http://www.google.com/bot.html)", "python-requests/2.32.3"}
	paths = []string{"/", "/index.html", "/login", "/api/users", "/api/orders", "/api/orders/1042", "/static/app.js",
		"/static/style.css", "/search?q=boots", "/admin", "/wp-login.php", "/health"}
	users = []string{"kim", "lee", "park", "choi", "jung", "-"}
)

func status(p string) int {
	switch {
	case p == "/admin" || p == "/wp-login.php":
		return pick([]int{401, 403, 404})
	case strings.HasPrefix(p, "/api/orders"):
		return pick([]int{200, 200, 200, 201, 500, 502, 504})
	}
	return pick([]int{200, 200, 200, 200, 304, 404})
}

func apacheAccess(start time.Time, n int) []byte {
	var b strings.Builder
	t := start
	for i := 0; i < n; i++ {
		t = t.Add(time.Duration(rnd.Intn(9000)) * time.Millisecond)
		p := pick(paths)
		m := "GET"
		if strings.HasPrefix(p, "/api/orders") && rnd.Intn(3) == 0 || p == "/login" && rnd.Intn(2) == 0 {
			m = "POST"
		}
		st := status(p)
		size := "-"
		if st != 304 {
			size = fmt.Sprint(200 + rnd.Intn(48000))
		}
		fmt.Fprintf(&b, "%s - %s [%s] \"%s %s HTTP/1.1\" %d %s \"%s\" \"%s\"\n",
			pick(ips), pick(users), t.Format("02/Jan/2006:15:04:05 -0700"), m, p, st, size,
			pick([]string{"-", "https://example.com/", "https://example.com/search"}), pick(agents))
	}
	return []byte(b.String())
}

func apacheError(start time.Time, n int) []byte {
	msgs := []string{
		"[core:error] [pid %d] [client %s:%d] AH00126: Invalid URI in request GET /%%%% HTTP/1.1",
		"[authz_core:error] [pid %d] [client %s:%d] AH01630: client denied by server configuration: /var/www/html/admin",
		"[proxy:error] [pid %d] [client %s:%d] AH00898: Error reading from remote server returned by /api/orders",
		"[proxy_http:error] [pid %d] [client %s:%d] AH01102: error reading status line from remote server 127.0.0.1:8080",
		"[ssl:warn] [pid %d] [client %s:%d] AH01909: server certificate does NOT include an ID which matches the server name",
		"[php:notice] [pid %d] [client %s:%d] PHP Notice:  Undefined index: page in /var/www/html/index.php on line 12",
	}
	var b strings.Builder
	t := start
	for i := 0; i < n; i++ {
		t = t.Add(time.Duration(rnd.Intn(120)) * time.Second)
		fmt.Fprintf(&b, "[%s] %s\n", t.Format("Mon Jan 02 15:04:05.000000 2006"),
			fmt.Sprintf(pick(msgs), 1000+rnd.Intn(9000), pick(ips), 40000+rnd.Intn(20000)))
	}
	return []byte(b.String())
}

func nginxAccess(start time.Time, n int) []byte {
	var b strings.Builder
	t := start
	for i := 0; i < n; i++ {
		t = t.Add(time.Duration(rnd.Intn(5000)) * time.Millisecond)
		p := pick(paths)
		st := status(p)
		rt := float64(rnd.Intn(300)) / 1000
		if st >= 500 {
			rt = 1 + float64(rnd.Intn(9000))/1000
		}
		fmt.Fprintf(&b, "%s - - [%s] \"GET %s HTTP/2.0\" %d %d \"-\" \"%s\" rt=%.3f uct=\"0.001\" uht=\"%.3f\" urt=\"%.3f\"\n",
			pick(ips), t.Format("02/Jan/2006:15:04:05 -0700"), p, st, 100+rnd.Intn(20000), pick(agents), rt, rt*0.9, rt*0.95)
	}
	return []byte(b.String())
}

func appJSON(start time.Time, n int) []byte {
	type ev struct{ level, msg string }
	evs := []ev{{"info", "request done"}, {"info", "request done"}, {"info", "cache hit"}, {"debug", "query plan chosen"},
		{"warn", "slow query"}, {"error", "db timeout"}, {"error", "payment declined"}, {"warn", "retrying upstream"}}
	var b strings.Builder
	t := start
	for i := 0; i < n; i++ {
		t = t.Add(time.Duration(rnd.Intn(3000)) * time.Millisecond)
		e := pick(evs)
		st, ms := 200, 5+rnd.Intn(120)
		switch e.level {
		case "warn":
			ms = 800 + rnd.Intn(2500)
		case "error":
			st, ms = pick([]int{500, 502, 503}), 1000+rnd.Intn(5000)
		}
		fmt.Fprintf(&b, "{\"ts\":\"%s\",\"level\":\"%s\",\"msg\":\"%s\",\"service\":\"%s\",\"req\":{\"method\":\"%s\",\"path\":\"%s\",\"id\":\"req-%05d\"},\"status\":%d,\"ms\":%d,\"user\":\"%s\"}\n",
			t.Format(time.RFC3339Nano), e.level, e.msg, pick([]string{"api", "api", "billing", "search"}),
			pick([]string{"GET", "GET", "POST"}), pick(paths[3:6]), rnd.Intn(100000), st, ms, pick(users[:5]))
	}
	return []byte(b.String())
}

func logfmt(start time.Time, n int) []byte {
	jobs := []string{"send_email", "resize_image", "sync_inventory", "charge_card", "rebuild_index"}
	var b strings.Builder
	t := start
	for i := 0; i < n; i++ {
		t = t.Add(time.Duration(rnd.Intn(4000)) * time.Millisecond)
		lvl, res, dur := "info", "ok", 20+rnd.Intn(900)
		if rnd.Intn(8) == 0 {
			lvl, res, dur = "error", pick([]string{"timeout", "conflict", "rejected"}), 3000+rnd.Intn(4000)
		}
		fmt.Fprintf(&b, "time=%s level=%s worker=w%d job=%s attempt=%d duration_ms=%d result=%s msg=\"job finished\"\n",
			t.Format(time.RFC3339), lvl, 1+rnd.Intn(4), pick(jobs), 1+rnd.Intn(3), dur, res)
	}
	return []byte(b.String())
}

func syslog(start time.Time, n int) []byte {
	lines := []string{
		"sshd[%d]: Accepted publickey for deploy from %s port %d ssh2",
		"sshd[%d]: Failed password for invalid user admin from %s port %d ssh2",
		"sshd[%d]: Connection closed by %s port %d [preauth]",
		"kernel: [%d.123456] TCP: request_sock_TCP: Possible SYN flooding on port 443. Sending cookies. src=%s port=%d",
		"CRON[%d]: (root) CMD (/usr/local/bin/backup.sh) from %s:%d",
		"systemd[1]: Started Session %d of user deploy. peer=%s:%d",
	}
	var b strings.Builder
	t := start
	for i := 0; i < n; i++ {
		t = t.Add(time.Duration(rnd.Intn(60)) * time.Second)
		fmt.Fprintf(&b, "%s web01 %s\n", t.Format("Jan _2 15:04:05"),
			fmt.Sprintf(pick(lines), 1000+rnd.Intn(30000), pick(ips), 30000+rnd.Intn(30000)))
	}
	return []byte(b.String())
}

// payments writes a custom format that no built-in sourcetype knows; the
// sample .plumb.conf defines its sourcetype.
func payments(start time.Time, n int) []byte {
	cards := []string{"visa", "master", "amex", "local"}
	var b strings.Builder
	t := start
	for i := 0; i < n; i++ {
		t = t.Add(time.Duration(rnd.Intn(6000)) * time.Millisecond)
		lvl, outcome := "INFO ", "approved"
		if rnd.Intn(7) == 0 {
			lvl, outcome = "WARN ", pick([]string{"declined", "declined", "fraud_check"})
		}
		fmt.Fprintf(&b, "%s %s [payment-svc] order %d charged %d KRW in %dms (card=%s) %s\n",
			t.Format("02.01.2006 15:04:05,000"), lvl, 1000+rnd.Intn(9000), (1+rnd.Intn(300))*100,
			40+rnd.Intn(900), pick(cards), outcome)
	}
	return []byte(b.String())
}

var fixed = time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.Header.ModTime = fixed
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func main() {
	root := os.Args[1]
	day := time.Date(2026, 10, 14, 9, 0, 0, 0, time.FixedZone("KST", 9*3600))
	w := func(name string, data []byte) {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			panic(err)
		}
	}
	w("apache/access.log", apacheAccess(day, 600))
	w("apache/access.log.1.gz", gz(apacheAccess(day.Add(-24*time.Hour), 600)))
	w("apache/error.log", apacheError(day, 150))
	w("nginx/access.log", nginxAccess(day, 400))
	w("app/app.jsonl", appJSON(day, 500))
	w("app/worker.log", logfmt(day, 300))
	w("syslog/messages", syslog(day, 300))

	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	add := func(name string, data []byte) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: fixed})
		tw.Write(data)
	}
	add("bundle/app/app.jsonl", appJSON(day.Add(-48*time.Hour), 400))
	add("bundle/app/app.jsonl.1.gz", gz(appJSON(day.Add(-72*time.Hour), 400)))
	add("bundle/web/access.log", apacheAccess(day.Add(-48*time.Hour), 400))
	add("bundle/system/messages", syslog(day.Add(-48*time.Hour), 200))
	tw.Close()
	w("archives/support-bundle.tar.gz", gz(tb.Bytes()))

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for _, name := range []string{"node-a/worker.log", "node-b/worker.log"} {
		f, _ := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: fixed})
		f.Write(logfmt(day.Add(-24*time.Hour), 200))
	}
	zw.Close()
	w("archives/workers.zip", zb.Bytes())

	w("payments/payments.log", payments(day, 300)) // last, so the files above stay the same
}

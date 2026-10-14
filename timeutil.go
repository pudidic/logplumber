package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Time support
//
// Every event has a _time field: the first timestamp found in the line, as Unix
// seconds (with milliseconds as a fraction). Recognized formats:
//
//	2026-10-14T09:00:00.555+09:00   ISO 8601 / RFC 3339 ('T' or space, ',' or '.' fraction, Z or offset)
//	2026/10/14 09:00:00             slashes instead of dashes (nginx error log)
//	14/Oct/2026:09:00:00 +0900      Apache/nginx access logs
//	Wed Oct 14 09:01:48.000 2026    ctime (Apache error log)
//	Oct 14 09:00:46                 BSD syslog (no year: the latest year that is not in the future)
//
// A timestamp without a zone is local time. When the line has none of these, a JSON
// or key=value field named time, ts, timestamp or @timestamp holding epoch seconds
// (or milliseconds) is used. A line without a timestamp has no _time.

const timeScanLimit = 256 // only the start of a line is searched

var monthAbbr = map[string]time.Month{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

// timeKeys are the fields checked for an epoch timestamp.
var timeKeys = []string{"time", "ts", "timestamp", "@timestamp", "t", "date", "datetime"}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// num reads exactly n digits at s[i:].
func num(s string, i, n int) (int, bool) {
	if i < 0 || i+n > len(s) {
		return 0, false
	}
	v := 0
	for k := i; k < i+n; k++ {
		if !isDigit(s[k]) {
			return 0, false
		}
		v = v*10 + int(s[k]-'0')
	}
	return v, true
}

// month reads a three-letter English month abbreviation at s[i:].
func month(s string, i int) (time.Month, bool) {
	if i+3 > len(s) {
		return 0, false
	}
	m, ok := monthAbbr[strings.ToLower(s[i:i+3])]
	if ok && i+3 < len(s) && isAlnum(s[i+3]) {
		return 0, false
	}
	return m, ok
}

// hms reads "hh:mm:ss" at s[i:] and an optional fraction; it returns the end offset.
func hms(s string, i int) (h, m, sec, nsec, end int, ok bool) {
	var o1, o2, o3 bool
	h, o1 = num(s, i, 2)
	m, o2 = num(s, i+3, 2)
	sec, o3 = num(s, i+6, 2)
	if !o1 || !o2 || !o3 || s[i+2] != ':' || s[i+5] != ':' || h > 23 || m > 59 || sec > 60 {
		return 0, 0, 0, 0, 0, false
	}
	end = i + 8
	if end+1 < len(s) && (s[end] == '.' || s[end] == ',') && isDigit(s[end+1]) {
		j, scale := end+1, 100000000
		for ; j < len(s) && isDigit(s[j]); j++ {
			nsec += int(s[j]-'0') * scale
			scale /= 10
		}
		end = j
	}
	return h, m, sec, nsec, end, true
}

// zone reads "Z", "+09:00", "+0900" or " +0900" at s[i:]; nil means local time.
func zone(s string, i int) *time.Location {
	if i < len(s) && s[i] == ' ' && i+1 < len(s) && (s[i+1] == '+' || s[i+1] == '-') {
		i++
	}
	if i >= len(s) {
		return nil
	}
	if s[i] == 'Z' {
		return time.UTC
	}
	if s[i] != '+' && s[i] != '-' {
		return nil
	}
	hh, ok := num(s, i+1, 2)
	if !ok {
		return nil
	}
	j := i + 3
	if j < len(s) && s[j] == ':' {
		j++
	}
	mm, ok := num(s, j, 2)
	if !ok {
		mm = 0
	}
	off := (hh*60 + mm) * 60
	if s[i] == '-' {
		off = -off
	}
	return time.FixedZone("", off)
}

func orLocal(l *time.Location) *time.Location {
	if l == nil {
		return time.Local
	}
	return l
}

// timeAt tries to read a timestamp starting at s[i:].
func timeAt(s string, i int, now time.Time) (time.Time, bool) {
	c := s[i]
	if isDigit(c) {
		// 2026-10-14T09:00:00 / 2026/10/14 09:00:00
		if y, ok := num(s, i, 4); ok && i+4 < len(s) && (s[i+4] == '-' || s[i+4] == '/') {
			sep := s[i+4]
			mo, ok1 := num(s, i+5, 2)
			d, ok2 := num(s, i+8, 2)
			if ok1 && ok2 && s[i+7] == sep && mo >= 1 && mo <= 12 && d >= 1 && d <= 31 && i+11 < len(s) &&
				(s[i+10] == 'T' || s[i+10] == ' ') {
				if h, m, sec, ns, end, ok := hms(s, i+11); ok {
					return time.Date(y, time.Month(mo), d, h, m, sec, ns, orLocal(zone(s, end))), true
				}
			}
			return time.Time{}, false
		}
		// 14/Oct/2026:09:00:00 +0900
		if d, ok := num(s, i, 2); ok && i+2 < len(s) && s[i+2] == '/' {
			mo, ok1 := month(s, i+3)
			y, ok2 := num(s, i+7, 4)
			if ok1 && ok2 && s[i+6] == '/' && i+11 < len(s) && s[i+11] == ':' {
				if h, m, sec, ns, end, ok := hms(s, i+12); ok {
					return time.Date(y, mo, d, h, m, sec, ns, orLocal(zone(s, end))), true
				}
			}
		}
		return time.Time{}, false
	}
	// Oct 14 09:00:46 [2026]   (also "Wed Oct 14 ...": the scan reaches the month)
	mo, ok := month(s, i)
	if !ok || i+4 >= len(s) || s[i+3] != ' ' {
		return time.Time{}, false
	}
	j := i + 4
	if s[j] == ' ' {
		j++
	}
	d := 0
	for k := j; k < len(s) && k < j+2 && isDigit(s[k]); k++ {
		d = d*10 + int(s[k]-'0')
		j = k + 1
	}
	if d < 1 || d > 31 || j >= len(s) || s[j] != ' ' {
		return time.Time{}, false
	}
	h, m, sec, ns, end, ok := hms(s, j+1)
	if !ok {
		return time.Time{}, false
	}
	if y, ok := num(s, end+1, 4); ok && s[end] == ' ' && (end+5 >= len(s) || !isAlnum(s[end+5])) {
		return time.Date(y, mo, d, h, m, sec, ns, time.Local), true
	}
	t := time.Date(now.Year(), mo, d, h, m, sec, ns, time.Local)
	if t.After(now.Add(48 * time.Hour)) {
		t = t.AddDate(-1, 0, 0)
	}
	return t, true
}

// findTime returns the first timestamp in the start of text.
func findTime(text string, now time.Time) (time.Time, bool) {
	s := text
	if len(s) > timeScanLimit {
		s = s[:timeScanLimit]
	}
	for i := 0; i+8 <= len(s); i++ {
		if i > 0 && isAlnum(s[i-1]) {
			continue
		}
		c := s[i]
		if !isDigit(c) && !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			continue
		}
		if t, ok := timeAt(s, i, now); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// epochTime reads a number as epoch seconds, or milliseconds when it is that large.
func epochTime(v string) (time.Time, bool) {
	f, ok := parseNum(v)
	if !ok || f <= 0 {
		return time.Time{}, false
	}
	if f > 1e11 {
		f /= 1000
	}
	return fromEpoch(f), true
}

func fromEpoch(f float64) time.Time {
	ms := int64(f*1000 + 0.5)
	return time.UnixMilli(ms)
}

func toEpoch(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

// epochString formats Unix seconds with millisecond precision, without trailing zeros.
func epochString(ms int64) string {
	sec, frac := ms/1000, ms%1000
	if frac < 0 {
		sec, frac = sec-1, frac+1000
	}
	if frac == 0 {
		return strconv.FormatInt(sec, 10)
	}
	return strings.TrimRight(fmt.Sprintf("%d.%03d", sec, frac), "0")
}

// formatTime renders a _time value for display.
func formatTime(v string) string {
	f, ok := parseNum(v)
	if !ok {
		return v
	}
	t := fromEpoch(f).Local()
	if t.Nanosecond() != 0 {
		return t.Format("2006-01-02 15:04:05.000")
	}
	return t.Format("2006-01-02 15:04:05")
}

// lineTime returns the _time of a line (Unix milliseconds). A _time set by eval
// or bin takes precedence over the timestamp in the text.
func (l *Line) lineTime() (int64, bool) {
	r := l.rec()
	if v, ok := r.set["_time"]; ok {
		f, ok := parseNum(v)
		return int64(math.Round(f * 1000)), ok
	}
	switch r.tState {
	case 1:
		return r.tm, true
	case 2:
		return 0, false
	}
	r.tState = 2
	if r.row {
		return 0, false
	}
	now := time.Now()
	t, ok := findTime(l.Text, now)
	if !ok {
		for _, k := range timeKeys {
			if v, has := l.Field(k); has {
				if t, ok = findTime(v, now); ok {
					break
				}
				if t, ok = epochTime(v); ok {
					break
				}
			}
		}
	}
	if !ok {
		return 0, false
	}
	r.tm, r.tState = t.UnixMilli(), 1
	return r.tm, true
}

// ---------------- spans ----------------

// span is a bucket size: n seconds, days or months.
type span struct {
	n    int64
	unit byte // 's' seconds, 'd' days, 'M' months
	text string
}

var spanUnits = map[string]struct {
	unit byte
	mult int64
}{
	"s": {'s', 1}, "sec": {'s', 1}, "secs": {'s', 1}, "second": {'s', 1}, "seconds": {'s', 1},
	"m": {'s', 60}, "min": {'s', 60}, "mins": {'s', 60}, "minute": {'s', 60}, "minutes": {'s', 60},
	"h": {'s', 3600}, "hr": {'s', 3600}, "hrs": {'s', 3600}, "hour": {'s', 3600}, "hours": {'s', 3600},
	"d": {'d', 1}, "day": {'d', 1}, "days": {'d', 1},
	"w": {'d', 7}, "week": {'d', 7}, "weeks": {'d', 7},
	"mon": {'M', 1}, "month": {'M', 1}, "months": {'M', 1},
	"q": {'M', 3}, "qtr": {'M', 3}, "quarter": {'M', 3}, "quarters": {'M', 3},
	"y": {'M', 12}, "yr": {'M', 12}, "year": {'M', 12}, "years": {'M', 12},
}

// parseSpan parses "30s", "5m", "1h", "1d", "1w", "1mon", "1y" or plain seconds.
func parseSpan(s string) (span, error) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if i == 0 {
		n, err = 1, nil
	}
	if err != nil || n <= 0 {
		return span{}, fmt.Errorf("invalid span %q (e.g. 30s, 5m, 1h, 1d, 1w, 1mon)", s)
	}
	unit := strings.ToLower(s[i:])
	if unit == "" {
		if i == 0 {
			return span{}, fmt.Errorf("invalid span %q (e.g. 30s, 5m, 1h, 1d, 1w, 1mon)", s)
		}
		unit = "s"
	}
	u, ok := spanUnits[unit]
	if !ok {
		return span{}, fmt.Errorf("invalid span unit %q (s m h d w mon q y)", s[i:])
	}
	return span{n: n * u.mult, unit: u.unit, text: s}, nil
}

func dayIndex(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// floor returns the start of the bucket that holds t. Buckets of whole days or
// less that divide a day are aligned to local midnight; weeks start on Sunday.
func (sp span) floor(t time.Time) time.Time {
	t = t.Local()
	switch sp.unit {
	case 's':
		if 86400%sp.n == 0 {
			y, m, d := t.Date()
			mid := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
			off := int64(t.Sub(mid) / time.Second)
			return mid.Add(time.Duration(off-off%sp.n) * time.Second)
		}
		u := t.Unix()
		return time.Unix(floorDiv(u, sp.n)*sp.n, 0)
	case 'd':
		idx := dayIndex(t)
		shift := int64(0)
		if sp.n%7 == 0 {
			shift = 4 // 1970-01-01 was a Thursday; weeks start on Sunday
		}
		idx = floorDiv(idx+shift, sp.n)*sp.n - shift
		return time.Date(1970, 1, 1+int(idx), 0, 0, 0, 0, time.Local)
	}
	idx := int64(t.Year())*12 + int64(t.Month()) - 1
	idx = floorDiv(idx, sp.n) * sp.n
	return time.Date(int(idx/12), time.Month(idx%12+1), 1, 0, 0, 0, 0, time.Local)
}

// next returns the start of the bucket after the one starting at t.
func (sp span) next(t time.Time) time.Time {
	switch sp.unit {
	case 's':
		return t.Add(time.Duration(sp.n) * time.Second)
	case 'd':
		return sp.floor(t.AddDate(0, 0, int(sp.n)).Add(time.Hour)) // the hour absorbs DST changes
	}
	return t.AddDate(0, int(sp.n), 0)
}

// autoSpans are the spans timechart picks from when no span is given; each divides the next.
var autoSpans = []span{
	{1, 's', "1s"}, {10, 's', "10s"}, {30, 's', "30s"}, {60, 's', "1m"}, {300, 's', "5m"},
	{600, 's', "10m"}, {1800, 's', "30m"}, {3600, 's', "1h"}, {10800, 's', "3h"},
	{43200, 's', "12h"}, {1, 'd', "1d"}, {7, 'd', "1w"},
}

// ---------------- relative time ----------------

// parseTimeSpec parses an earliest/latest value relative to now: "now", "0",
// epoch seconds, an absolute time ("2026-10-14T09:00:00", "2026-10-14",
// "10/14/2026:09:00:00"), or a relative time made of offsets and snaps such as
// "-24h", "-1d@d", "@h", "-7d@w0", "@d+9h".
func parseTimeSpec(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "now", "":
		return now, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && s[0] != '+' && s[0] != '-' {
		return fromEpoch(f), nil
	}
	if t, ok := absTime(s); ok {
		return t, nil
	}
	t := now
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '+' || c == '-':
			j := i + 1
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			k := j
			for k < len(s) && (s[k] >= 'a' && s[k] <= 'z' || s[k] >= 'A' && s[k] <= 'Z') {
				k++
			}
			sp, err := parseSpan(s[i+1 : k])
			if err != nil || k == j {
				return t, fmt.Errorf("invalid time %q: offset like -15m, -24h or -7d expected", s)
			}
			n := sp.n
			if c == '-' {
				n = -n
			}
			switch sp.unit {
			case 's':
				t = t.Add(time.Duration(n) * time.Second)
			case 'd':
				t = t.AddDate(0, 0, int(n))
			default:
				t = t.AddDate(0, int(n), 0)
			}
			i = k
		case c == '@':
			j := i + 1
			for j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') {
				j++
			}
			unit := strings.ToLower(s[i+1 : j])
			wd := -1
			if (unit == "w" || unit == "week") && j < len(s) && s[j] >= '0' && s[j] <= '6' {
				wd = int(s[j] - '0')
				j++
			}
			var err error
			if t, err = snapTime(t, unit, wd); err != nil {
				return t, fmt.Errorf("invalid time %q: %v", s, err)
			}
			i = j
		default:
			return t, fmt.Errorf("invalid time %q (e.g. -24h, -7d@d, @h, now, 2026-10-14T09:00:00)", s)
		}
	}
	return t, nil
}

// snapTime rounds t down to the start of a unit ("@d"); wd is the weekday of "@w1".
func snapTime(t time.Time, unit string, wd int) (time.Time, error) {
	t = t.Local()
	u, ok := spanUnits[unit]
	if !ok {
		return t, fmt.Errorf("unknown snap unit @%s", unit)
	}
	switch {
	case u.unit == 'd' && u.mult == 7:
		if wd < 0 {
			wd = 0
		}
		d := span{n: 1, unit: 'd'}.floor(t)
		back := (int(d.Weekday()) - wd + 7) % 7
		return d.AddDate(0, 0, -back), nil
	case u.unit == 'M' && u.mult == 12:
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.Local), nil
	}
	return span{n: u.mult, unit: u.unit}.floor(t), nil
}

// absTime parses an absolute time given on the command line.
func absTime(s string) (time.Time, bool) {
	if t, ok := timeAt(s, 0, time.Now()); ok && len(s) >= 19 {
		return t, true
	}
	for _, layout := range []string{"2006-01-02", "01/02/2006:15:04:05", "01/02/2006", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ---------------- strftime / strptime ----------------

// strftime formats t with C-style % codes.
func strftime(t time.Time, f string) string {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' || i+1 >= len(f) {
			b.WriteByte(f[i])
			continue
		}
		i++
		switch f[i] {
		case 'Y':
			b.WriteString(t.Format("2006"))
		case 'y':
			b.WriteString(t.Format("06"))
		case 'm':
			b.WriteString(t.Format("01"))
		case 'd':
			b.WriteString(t.Format("02"))
		case 'e':
			b.WriteString(t.Format("_2"))
		case 'H':
			b.WriteString(t.Format("15"))
		case 'I':
			b.WriteString(t.Format("03"))
		case 'M':
			b.WriteString(t.Format("04"))
		case 'S':
			b.WriteString(t.Format("05"))
		case 'p':
			b.WriteString(t.Format("PM"))
		case 'b', 'h':
			b.WriteString(t.Format("Jan"))
		case 'B':
			b.WriteString(t.Format("January"))
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'A':
			b.WriteString(t.Format("Monday"))
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'z':
			b.WriteString(t.Format("-0700"))
		case 'Z':
			b.WriteString(t.Format("MST"))
		case 's':
			b.WriteString(strconv.FormatInt(t.Unix(), 10))
		case 'F':
			b.WriteString(t.Format("2006-01-02"))
		case 'T':
			b.WriteString(t.Format("15:04:05"))
		case 'w':
			b.WriteString(strconv.Itoa(int(t.Weekday())))
		case 'f':
			fmt.Fprintf(&b, "%06d", t.Nanosecond()/1000)
		case 'N':
			fmt.Fprintf(&b, "%09d", t.Nanosecond())
		case '3', '6', '9':
			if i+1 < len(f) && f[i+1] == 'N' {
				d := int(f[i] - '0')
				ns := fmt.Sprintf("%09d", t.Nanosecond())
				b.WriteString(ns[:d])
				i++
			} else {
				b.WriteByte('%')
				b.WriteByte(f[i])
			}
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(f[i])
		}
	}
	return b.String()
}

var strptimeLayout = map[byte]string{
	'Y': "2006", 'y': "06", 'm': "01", 'd': "02", 'e': "_2", 'H': "15", 'I': "03", 'M': "04",
	'S': "05", 'p': "PM", 'b': "Jan", 'h': "Jan", 'B': "January", 'a': "Mon", 'A': "Monday",
	'z': "-0700", 'Z': "MST", 'F': "2006-01-02", 'T': "15:04:05", 'f': "000000", 'N': "000000000",
}

// strptime parses s with C-style % codes.
func strptime(s, f string) (time.Time, bool) {
	if f == "%s" {
		return epochTime(s)
	}
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' || i+1 >= len(f) {
			b.WriteByte(f[i])
			continue
		}
		i++
		if (f[i] == '3' || f[i] == '6' || f[i] == '9') && i+1 < len(f) && f[i+1] == 'N' {
			b.WriteString(strings.Repeat("0", int(f[i]-'0')))
			i++
			continue
		}
		if f[i] == '%' {
			b.WriteByte('%')
			continue
		}
		l, ok := strptimeLayout[f[i]]
		if !ok {
			return time.Time{}, false
		}
		b.WriteString(l)
	}
	t, err := time.ParseInLocation(b.String(), s, time.Local)
	return t, err == nil
}

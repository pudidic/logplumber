package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Timeline: above the events of a search, one line shows how many events fall
// into each time slot, with time running left to right (Splunk's event timeline):
//
//	09:00 ▁▂▃▅▇█▆▅▃▂▁ ▂▃▅ 11:38 · 5m/col · max 42 · 1,234 events
//
// It counts every matching event, also the ones a preview or head leaves out.

const timelineMaxBuckets = 8192 // stored buckets before switching to a coarser span

var timelineSpans = append(append([]span(nil), autoSpans...),
	span{1, 'M', "1mon"}, span{3, 'M', "3mon"}, span{12, 'M', "1y"})

type timeline struct {
	mu      sync.Mutex
	store   int             // index into autoSpans of the stored buckets
	counts  map[int64]int64 // bucket start (Unix seconds) -> events
	total   int64
	noTime  int64
	minT    int64 // Unix seconds
	maxT    int64
	changed bool
}

func newTimeline() *timeline { return &timeline{counts: map[int64]int64{}} }

func (t *timeline) add(ms int64, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.changed = true
	if !ok {
		t.noTime++
		return
	}
	sec := ms / 1000
	if t.total == 0 || sec < t.minT {
		t.minT = sec
	}
	if t.total == 0 || sec > t.maxT {
		t.maxT = sec
	}
	t.total++
	t.counts[autoSpans[t.store].floor(time.Unix(sec, 0)).Unix()]++
	if len(t.counts) > timelineMaxBuckets && t.store < len(autoSpans)-1 {
		t.store++
		sp := autoSpans[t.store]
		old := t.counts
		t.counts = make(map[int64]int64, len(old)/2)
		for k, c := range old {
			t.counts[sp.floor(time.Unix(k, 0)).Unix()] += c
		}
	}
}

var sparkLevels = []rune("▁▂▃▄▅▆▇█")

// timelineTrim is the share of events at each end that may fall outside the
// range shown, so a few stray timestamps (years away) do not squeeze the rest
// into one column. It applies from timelineTrimMin events on.
const (
	timelineTrim    = 0.005
	timelineTrimMin = 200
)

// span returns the time range to show and how many events lie outside it.
// Called with mu held.
func (t *timeline) span() (lo, hi time.Time, outside int64) {
	lo, hi = time.Unix(t.minT, 0), time.Unix(t.maxT, 0)
	if t.total < timelineTrimMin {
		return lo, hi, 0
	}
	keys := make([]int64, 0, len(t.counts))
	for k := range t.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	cut := int64(float64(t.total) * timelineTrim)
	var acc int64
	first, last := 0, len(keys)-1
	for first < last && acc+t.counts[keys[first]] <= cut {
		acc += t.counts[keys[first]]
		first++
	}
	acc = 0
	for last > first && acc+t.counts[keys[last]] <= cut {
		acc += t.counts[keys[last]]
		last--
	}
	// The core holds all but the outermost events. Events within one core width
	// of it are still shown; only ones far away are left out.
	width := keys[last] - keys[first] + autoSpans[t.store].n
	for first > 0 && keys[first-1] >= keys[first]-width {
		first--
	}
	for last < len(keys)-1 && keys[last+1] <= keys[last]+width {
		last++
	}
	if first == 0 && last == len(keys)-1 {
		return lo, hi, 0
	}
	for i, k := range keys {
		if i < first || i > last {
			outside += t.counts[k]
		}
	}
	if first > 0 {
		lo = time.Unix(keys[first], 0)
	}
	if last < len(keys)-1 {
		hi = autoSpans[t.store].next(time.Unix(keys[last], 0)).Add(-time.Second)
	}
	return lo, hi, outside
}

// render draws the timeline in at most width columns ("" before any event).
func (t *timeline) render(width int) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.total == 0 {
		if t.noTime > 0 {
			return fmt.Sprintf("timeline: %s events without a timestamp", commas(t.noTime))
		}
		return ""
	}
	lo, hi, outside := t.span()
	sameDay := lo.Local().Format("20060102") == hi.Local().Format("20060102")
	// The text after the bars, from complete to short for narrow terminals.
	tail := func(level int, sp span, peak int64, end string) string {
		s := " " + end
		if level >= 1 {
			s += fmt.Sprintf(" · %s/col · max %s", sp.text, commas(peak))
		}
		if level >= 2 {
			s += fmt.Sprintf(" · %s events", commas(t.total))
			if outside > 0 {
				s += fmt.Sprintf(" (%s outside)", commas(outside))
			}
		}
		if level >= 3 && t.noTime > 0 {
			s += fmt.Sprintf(" (+%s without time)", commas(t.noTime))
		}
		return s
	}
	const minBars = 12
	// the finest span (not finer than the stored buckets) whose columns fit
	var sp span
	var starts []time.Time
	room, level := 1, 0
	for i, cand := range timelineSpans {
		if i < t.store {
			continue
		}
		sp = cand
		start, end := timeLabel(lo, cand, sameDay), timeLabel(hi, cand, sameDay)
		for level = 3; level >= 0; level-- { // the longest tail that leaves room for bars
			room = width - 1 - strWidth([]rune(start)) - 1 - strWidth([]rune(tail(level, cand, t.total, end)))
			if room >= minBars {
				break
			}
		}
		if room < 1 {
			continue
		}
		starts = starts[:0]
		for c := cand.floor(lo); !c.After(hi) && len(starts) <= room; c = cand.next(c) {
			starts = append(starts, c)
		}
		if len(starts) <= room {
			break
		}
	}
	level = max(level, 0)
	if len(starts) == 0 {
		starts = []time.Time{sp.floor(lo)}
	}
	if len(starts) > max(room, 1) { // nothing fits: the earliest columns
		starts = starts[:max(room, 1)]
	}
	cols := make([]int64, len(starts))
	idx := make(map[int64]int, len(starts))
	for i, s := range starts {
		idx[s.Unix()] = i
	}
	for k, c := range t.counts {
		if i, ok := idx[sp.floor(time.Unix(k, 0)).Unix()]; ok && k >= lo.Unix()-int64(autoSpans[t.store].n) && !time.Unix(k, 0).After(hi) {
			cols[i] += c
		}
	}
	var peak int64
	for _, c := range cols {
		peak = max(peak, c)
	}
	var b strings.Builder
	b.WriteString(timeLabel(lo, sp, sameDay))
	b.WriteByte(' ')
	for _, c := range cols {
		if c == 0 {
			b.WriteByte(' ')
			continue
		}
		lvl := int((c*int64(len(sparkLevels)) + peak - 1) / peak) // 1..8, rounded up
		b.WriteRune(sparkLevels[max(1, min(lvl, len(sparkLevels)))-1])
	}
	b.WriteString(tail(level, sp, peak, timeLabel(hi, sp, sameDay)))
	return b.String()
}

// timeLabel formats a timeline end: as precise as the span needs.
func timeLabel(t time.Time, sp span, sameDay bool) string {
	t = t.Local()
	switch {
	case sp.unit == 'M' || sp.unit == 'd':
		return t.Format("2006-01-02")
	case sp.n < 60 && sameDay:
		return t.Format("15:04:05")
	case sameDay:
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// timelineStage counts the events that pass through it.
type timelineStage struct{ t *timeline }

func (s timelineStage) Push(l Line, emit Emit) bool {
	s.t.add(l.lineTime())
	return emit(l)
}
func (timelineStage) Finish(Emit) {}

func timelineSpec(t *timeline) *Spec {
	return &Spec{Name: "timeline", New: func() Stage { return timelineStage{t: t} }}
}

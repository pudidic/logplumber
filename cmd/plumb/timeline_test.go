package main

import (
	"strings"
	"testing"
	"time"
)

func TestTimelineRender(t *testing.T) {
	utc(t)
	tl := newTimeline()
	if tl.render(80) != "" {
		t.Error("empty timeline should render nothing")
	}
	base := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	// 1, 2, 4, 8 events in four consecutive minutes, then a gap, then 8
	for i, n := range []int{1, 2, 4, 8, 0, 8} {
		for k := 0; k < n; k++ {
			tl.add(base.Add(time.Duration(i)*time.Minute+time.Duration(k)*time.Second).UnixMilli(), true)
		}
	}
	tl.add(0, false)
	got := tl.render(100)
	// the finest span that fits: 10 s per column, minutes 1..6 at columns 0, 6, 12, ...
	want := "09:00:00 ▁     ▂     ▄     █           █ 09:05:07 · 10s/col · max 8 · 23 events (+1 without time)"
	if got != want {
		t.Errorf("render:\n got  %q\n want %q", got, want)
	}
	// narrower terminals: a coarser span and a shorter text after the bars
	for _, w := range []int{80, 60, 40, 25} {
		got = tl.render(w)
		if strWidth([]rune(got)) > w-1 || strings.Count(got, "█") < 1 {
			t.Errorf("width %d: %q (%d columns)", w, got, strWidth([]rune(got)))
		}
	}
	if got := tl.render(60); got != "09:00:00 ▁ ▂ ▄ █   █ 09:05:07 · 30s/col · max 8" { // finer columns win over the longer text
		t.Errorf("width 60: %q", got)
	}
	// several days: dates in the labels
	tl2 := newTimeline()
	tl2.add(base.UnixMilli(), true)
	tl2.add(base.AddDate(0, 0, 3).UnixMilli(), true)
	if got := tl2.render(80); !strings.HasPrefix(got, "10-14 ") && !strings.HasPrefix(got, "2026-10-14 ") {
		t.Errorf("days: %q", got)
	}
	// only events without a timestamp
	tl3 := newTimeline()
	tl3.add(0, false)
	if got := tl3.render(80); got != "timeline: 1 events without a timestamp" {
		t.Errorf("no time: %q", got)
	}
}

func TestTimelineCoarsens(t *testing.T) {
	tl := newTimeline()
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC).UnixMilli()
	for i := 0; i < 3*86400; i += 7 { // three days, one event every 7 s
		tl.add(base+int64(i)*1000, true)
	}
	if tl.store == 0 || len(tl.counts) > timelineMaxBuckets {
		t.Errorf("stored %d buckets at span %s", len(tl.counts), autoSpans[tl.store].text)
	}
	got := tl.render(100)
	if !strings.Contains(got, "37,029 events") || strWidth([]rune(got)) > 99 {
		t.Errorf("render: %q", got)
	}
}

func TestTimelineCountsAll(t *testing.T) {
	// head 2 shows two events; the timeline counts all matching ones
	ast, _ := splParse("x | head 2")
	var specs []*Spec
	for i, c := range ast.cmds {
		s, _, _ := buildSPLSpec(c, i == 0)
		specs = append(specs, s)
	}
	specs, tl := orderSPL(specs, false, nil)
	out := runSpecs(specs, "2026-10-14T09:00:00Z x", "2026-10-14T09:00:01Z x", "2026-10-14T09:00:02Z x", "2026-10-14T09:00:03Z y")
	if len(out) != 2 || tl == nil || tl.total != 3 {
		t.Errorf("out %q, timeline total %v", out, tl)
	}
}

func TestTimelineTrimsStrays(t *testing.T) {
	utc(t)
	tl := newTimeline()
	base := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 600; i++ {
		tl.add(base.Add(time.Duration(i)*time.Second).UnixMilli(), true)
	}
	tl.add(time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC).UnixMilli(), true) // a stray timestamp
	got := tl.render(100)
	if !strings.HasPrefix(got, "09:00") || !strings.Contains(got, "601 events (1 outside)") {
		t.Errorf("stray: %q", got)
	}
}

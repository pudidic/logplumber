package main

import (
	"encoding/binary"
	"errors"
	"os"
	"sort"
)

// Line encoding for spilling to disk: text, source, line number and the fields set
// by rex/eval/aggregations. Automatically extracted fields are not stored; they are
// extracted again from the text when needed.

func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func encodeLine(b []byte, l Line) []byte {
	b = appendString(b, l.Text)
	b = appendString(b, l.Src)
	b = binary.AppendUvarint(b, uint64(l.No))
	var set map[string]string
	if l.Rec != nil {
		set = l.Rec.set
	}
	b = binary.AppendUvarint(b, uint64(len(set)))
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b = appendString(b, k)
		b = appendString(b, set[k])
	}
	if l.Rec != nil && l.Rec.row {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	return b
}

var errCorrupt = errors.New("corrupt spill data")

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = errCorrupt
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) str() string {
	n := d.uvarint()
	if d.err != nil || uint64(len(d.b)) < n {
		d.err = errCorrupt
		return ""
	}
	s := string(d.b[:n])
	d.b = d.b[n:]
	return s
}

// decodeLine decodes one line; header is attached to aggregation rows.
func decodeLine(b []byte, header *tableHeader) (Line, error) {
	d := &decoder{b: b}
	l := Line{Text: d.str(), Src: d.str(), No: int(d.uvarint())}
	if n := d.uvarint(); n > 0 && d.err == nil {
		l.Rec = &Record{set: make(map[string]string, n)}
		for i := uint64(0); i < n && d.err == nil; i++ {
			k := d.str()
			l.Rec.set[k] = d.str()
		}
	}
	if d.err == nil && len(d.b) > 0 && d.b[0] == 1 {
		if l.Rec == nil {
			l.Rec = &Record{}
		}
		l.Rec.row, l.Rec.header = true, header
	}
	return l, d.err
}

// rowLog keeps the results of one type-ahead run for browsing. The first memRows
// lines stay in memory; the rest go to a temporary file with only their offsets
// kept in memory.
type rowLog struct {
	dir    string // where the spill file goes ("" = OS temp directory)
	mem    []Line
	f      *os.File
	offs   []int64 // offsets of spilled lines; offs[i+1]-offs[i] is the length
	end    int64
	header *tableHeader
	buf    []byte
	err    error
}

const memRows = 2000

func (r *rowLog) len() int { return len(r.mem) + len(r.offs) }

func (r *rowLog) add(l Line) {
	if h := lineHeader(l); h != nil && r.header == nil {
		r.header = h
	}
	if len(r.mem) < memRows {
		r.mem = append(r.mem, l)
		return
	}
	if r.err != nil {
		return
	}
	if r.f == nil {
		if r.f, r.err = os.CreateTemp(r.dir, "rows-*.bin"); r.err != nil {
			return
		}
	}
	r.buf = encodeLine(r.buf[:0], l)
	r.buf = binary.AppendUvarint(r.buf, 0) // keeps every record non-empty
	if _, r.err = r.f.WriteAt(r.buf, r.end); r.err != nil {
		return
	}
	r.offs = append(r.offs, r.end)
	r.end += int64(len(r.buf))
}

// get returns line i (0-based).
func (r *rowLog) get(i int) (Line, bool) {
	if i < 0 || i >= r.len() {
		return Line{}, false
	}
	if i < len(r.mem) {
		return r.mem[i], true
	}
	j := i - len(r.mem)
	end := r.end
	if j+1 < len(r.offs) {
		end = r.offs[j+1]
	}
	b := make([]byte, end-r.offs[j])
	if _, err := r.f.ReadAt(b, r.offs[j]); err != nil {
		return Line{}, false
	}
	l, err := decodeLine(b, r.header)
	return l, err == nil
}

// close deletes the spill file.
func (r *rowLog) close() {
	if r.f != nil {
		name := r.f.Name()
		r.f.Close()
		os.Remove(name)
		r.f = nil
	}
}

package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
)

// leafFunc handles one plain (non-archive) stream found while unpacking a file.
// member is the "::path" suffix of an archive member ("" for the file itself),
// logical is the base name used by name filters, and decoded tells whether the
// bytes were decompressed (so they differ from the file on disk).
type leafFunc func(member, logical string, br *bufio.Reader, decoded bool) error

// archiveWalker unpacks gzip, bzip2, zip and tar data recursively and calls leaf
// for every plain stream inside. It is shared by queries and the indexer.
type archiveWalker struct {
	ctx   context.Context
	disp  string // display name of the outer file, for error messages
	leaf  leafFunc
	onErr func(error)
}

// walkFile unpacks a file on disk. ra and size are used for a zip at the top level
// (zip needs random access); r is the sequential reader for everything else.
func (a *archiveWalker) walkFile(ra io.ReaderAt, size int64, r io.Reader) error {
	var magic [4]byte
	if n, _ := ra.ReadAt(magic[:], 0); n == 4 && string(magic[:]) == "PK\x03\x04" {
		zr, err := zip.NewReader(ra, size)
		if err != nil {
			a.onErr(fmt.Errorf("%s: %v", a.disp, err))
			return nil
		}
		return a.zipMembers("", zr, 0)
	}
	return a.stream("", path.Base(a.disp), r, 0, false)
}

func (a *archiveWalker) zipMembers(member string, zr *zip.Reader, depth int) error {
	for _, zf := range zr.File {
		if strings.HasSuffix(zf.Name, "/") {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			a.onErr(fmt.Errorf("%s%s::%s: %v", a.disp, member, zf.Name, err))
			continue
		}
		err = a.stream(member+"::"+zf.Name, path.Base(zf.Name), rc, depth+1, true)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// stream sniffs r by its leading bytes and unpacks it, or hands it to leaf.
func (a *archiveWalker) stream(member, logical string, r io.Reader, depth int, decoded bool) error {
	if a.ctx.Err() != nil {
		return errStop
	}
	br := bufio.NewReaderSize(r, 256<<10)
	head, _ := br.Peek(512)
	if depth < maxNestDepth {
		switch {
		case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
			gz, err := gzip.NewReader(br)
			if err != nil {
				a.onErr(fmt.Errorf("%s%s: %v", a.disp, member, err))
				return nil
			}
			defer gz.Close()
			inner := logical
			if l := strings.ToLower(inner); strings.HasSuffix(l, ".tgz") {
				inner = inner[:len(inner)-4] + ".tar"
			} else if strings.HasSuffix(l, ".gz") {
				inner = inner[:len(inner)-3]
			}
			return a.stream(member, inner, gz, depth+1, true)
		case len(head) >= 4 && string(head[:3]) == "BZh" && head[3] >= '1' && head[3] <= '9':
			inner := logical
			if l := strings.ToLower(inner); strings.HasSuffix(l, ".tbz2") || strings.HasSuffix(l, ".tbz") {
				inner = inner[:strings.LastIndexByte(inner, '.')] + ".tar"
			} else if strings.HasSuffix(l, ".bz2") {
				inner = inner[:len(inner)-4]
			}
			return a.stream(member, inner, bzip2.NewReader(br), depth+1, true)
		case len(head) >= 4 && string(head[:4]) == "PK\x03\x04":
			data, err := io.ReadAll(io.LimitReader(br, maxInMemoryZip+1))
			if err != nil || len(data) > maxInMemoryZip {
				a.onErr(fmt.Errorf("%s%s: nested zip too large or unreadable", a.disp, member))
				return nil
			}
			zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				a.onErr(fmt.Errorf("%s%s: %v", a.disp, member, err))
				return nil
			}
			return a.zipMembers(member, zr, depth)
		case len(head) >= 262 && string(head[257:262]) == "ustar":
			tr := tar.NewReader(br)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					a.onErr(fmt.Errorf("%s%s: %v", a.disp, member, err))
					return nil
				}
				if !hdr.FileInfo().Mode().IsRegular() {
					continue
				}
				name := strings.TrimPrefix(hdr.Name, "./")
				if err := a.stream(member+"::"+name, path.Base(name), tr, depth+1, true); err != nil {
					return err
				}
			}
		}
	}
	return a.leaf(member, logical, br, decoded)
}

// isBinary reports whether the stream looks binary (a NUL byte near the start).
func isBinary(br *bufio.Reader) bool {
	p, _ := br.Peek(8000)
	return bytes.IndexByte(p, 0) >= 0
}

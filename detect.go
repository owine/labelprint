package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type docKind string

const (
	kindZPL  docKind = "zpl"
	kindPDF  docKind = "pdf"
	kindPNG  docKind = "png"
	kindJPEG docKind = "jpeg"
)

// mimeType is what LPrint is told the document is (document-format).
func (k docKind) mimeType() string {
	switch k {
	case kindZPL:
		return "application/vnd.zebra-zpl"
	case kindPNG:
		return "image/png"
	case kindJPEG:
		return "image/jpeg"
	}
	return "" // PDFs are never sent as-is
}

// userError is a problem with the upload itself, shown to the user verbatim
// with a 4xx status.
type userError struct {
	Status int
	Msg    string
}

func (e *userError) Error() string { return e.Msg }

func badRequest(format string, a ...any) *userError {
	return &userError{Status: 400, Msg: fmt.Sprintf(format, a...)}
}

// detectKind sniffs content first and trusts the extension only for ZPL, which
// has no magic number. A .zpl file is accepted as-is (it may hold only ~
// commands, like ~JC to calibrate); any other text must contain a ^XA … ^XZ
// label to count as ZPL.
func detectKind(filename string, data []byte) (docKind, error) {
	head := data[:min(len(data), 1024)]
	switch {
	case len(data) == 0:
		return "", badRequest("that file is empty")
	case bytes.Contains(head, []byte("%PDF-")):
		return kindPDF, nil
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return kindPNG, nil
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return kindJPEG, nil
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == ".zpl" || containsZPLLabel(data) {
		return kindZPL, nil
	}
	if ext == ".txt" {
		return "", &userError{Status: 415, Msg: "that .txt file has no ZPL label in it (no ^XA … ^XZ)"}
	}
	return "", &userError{Status: 415, Msg: "unsupported file — send ZPL (.zpl/.txt), PDF, PNG or JPEG"}
}

// containsZPLLabel reports whether data has a ^XA that is later closed by ^XZ.
// ZPL commands are case-insensitive.
func containsZPLLabel(data []byte) bool {
	start := indexFold(data, "^XA")
	return start >= 0 && indexFold(data[start+3:], "^XZ") >= 0
}

// indexFold finds a 3-byte ASCII command regardless of case without copying data.
func indexFold(data []byte, cmd string) int {
	for i := 0; ; {
		j := bytes.IndexByte(data[i:], cmd[0])
		if j < 0 || i+j+len(cmd) > len(data) {
			return -1
		}
		i += j
		if strings.EqualFold(string(data[i:i+len(cmd)]), cmd) {
			return i
		}
		i++
	}
}

// countZPLLabels is shown in the preview ("3 labels").
func countZPLLabels(data []byte) int {
	n := 0
	for {
		i := indexFold(data, "^XA")
		if i < 0 {
			return n
		}
		n++
		data = data[i+3:]
	}
}

// parsePageRange turns "1-3,5,8-" into sorted, de-duplicated page numbers
// within 1..total. Empty or "all" selects every page.
func parsePageRange(spec string, total int) ([]int, error) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if spec == "" || spec == "all" {
		pages := make([]int, total)
		for i := range pages {
			pages[i] = i + 1
		}
		return pages, nil
	}
	var pages []int
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := pageNum(lo, 1)
		if err != nil {
			return nil, badRequest("can't read page %q in %q", part, spec)
		}
		last := first
		if isRange {
			if last, err = pageNum(hi, total); err != nil {
				return nil, badRequest("can't read page %q in %q", part, spec)
			}
		}
		if first < 1 || last > total || first > last {
			return nil, badRequest("pages %q are outside this pdf's 1–%d", part, total)
		}
		for p := first; p <= last; p++ {
			pages = append(pages, p)
		}
	}
	if len(pages) == 0 {
		return nil, badRequest("no pages selected by %q", spec)
	}
	slices.Sort(pages)
	return slices.Compact(pages), nil
}

func pageNum(s string, dflt int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return dflt, nil
	}
	return strconv.Atoi(s)
}

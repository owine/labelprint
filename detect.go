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
// with a 4xx status. Field names the form field at fault, if any, so the page
// can flag that input.
type userError struct {
	Status int
	Msg    string
	Field  string
}

func (e *userError) Error() string { return e.Msg }

func badRequest(format string, a ...any) *userError {
	return &userError{Status: 400, Msg: fmt.Sprintf(format, a...)}
}

func fieldError(field, format string, a ...any) *userError {
	return &userError{Status: 400, Msg: fmt.Sprintf(format, a...), Field: field}
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
	// A ^XA … ^XZ label wins; EPL2 is checked before the .zpl extension so an
	// EPL2 body saved as .zpl is still refused (LPrint would print it as text).
	ext := strings.ToLower(filepath.Ext(filename))
	if containsZPLLabel(data) {
		return kindZPL, nil
	}
	if looksLikeEPL2(data) {
		return "", errEPL2
	}
	if ext == ".zpl" {
		return kindZPL, nil
	}
	if ext == ".txt" {
		return "", &userError{Status: 415, Msg: "that .txt file has no ZPL label in it (no ^XA … ^XZ)"}
	}
	return "", &userError{Status: 415, Msg: "unsupported file — send ZPL (.zpl/.txt), PDF, PNG or JPEG"}
}

// errEPL2 is what an EPL2 upload gets. UPS and FedEx offer EPL2 next to ZPL,
// and LPrint doesn't take it, so it's refused rather than passed on as ZPL.
var errEPL2 = &userError{Status: 415, Msg: "EPL2 isn't supported — choose ZPL or PDF in your shipping tool"}

// looksLikeEPL2 reports whether data starts like an EPL2 label. Within the
// first few non-empty lines there is an "N" line (clear buffer), preceded only
// by EPL2 setup lines (I8,A,001 / OD / JF / ZB / q812 ...), and followed within
// a few lines by an EPL2 command with arguments (A50,50,..., B..., P1) or
// ZT/ZB. Requiring both keeps plain text and CSV that merely contain "N" out.
func looksLikeEPL2(data []byte) bool {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	data = data[:min(len(data), 2048)]
	seen, nAt := 0, 0
	for line := range bytes.Lines(data) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		seen++
		switch {
		case nAt == 0 && string(line) == "N":
			nAt = seen
		case nAt == 0:
			if seen >= 8 || !epl2Setup(line) {
				return false
			}
		default:
			if epl2Command(line) {
				return true
			}
			if seen-nAt >= 5 {
				return false
			}
		}
	}
	return false
}

// epl2Setup matches the lines EPL2 files put before N: the commands below,
// I (code page, "I8,A,001"), O (options, "OD", "OEPL1": uppercase letters,
// digits and commas only) and JF/JB.
func epl2Setup(line []byte) bool {
	if epl2Command(line) || string(line) == "JF" || string(line) == "JB" {
		return true
	}
	switch line[0] {
	case 'I':
		return len(line) > 1 && line[1] >= '0' && line[1] <= '9'
	case 'O':
		for _, c := range line[1:] {
			if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != ',' {
				return false
			}
		}
		return len(line) <= 12
	}
	return false
}

// epl2Command matches a command letter (or two) followed by a digit, or the
// argument-less ZT/ZB.
func epl2Command(line []byte) bool {
	if string(line) == "ZT" || string(line) == "ZB" {
		return true
	}
	n := 1
	if bytes.HasPrefix(line, []byte("LO")) || bytes.HasPrefix(line, []byte("GW")) {
		n = 2
	} else if !strings.ContainsRune("qQSDABbPRO", rune(line[0])) {
		return false
	}
	return len(line) > n && line[n] >= '0' && line[n] <= '9'
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
			return nil, fieldError("pages", "can't read page %q in %q", part, spec)
		}
		last := first
		if isRange {
			if last, err = pageNum(hi, total); err != nil {
				return nil, fieldError("pages", "can't read page %q in %q", part, spec)
			}
		}
		if first < 1 || last > total || first > last {
			return nil, fieldError("pages", "pages %q are outside this pdf's 1–%d", part, total)
		}
		for p := first; p <= last; p++ {
			pages = append(pages, p)
		}
	}
	if len(pages) == 0 {
		return nil, fieldError("pages", "no pages selected by %q", spec)
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

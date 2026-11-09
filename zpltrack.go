package main

import (
	"regexp"
	"strings"
)

// ZPL tracking numbers come from the source, not a decode: the printer draws
// the barcode itself from ^FD, so the field data is what it will encode.
// Only ^BC (Code 128) and ^B3 (Code 39) fields count. Fields under ^FH (hex
// escapes) or ^FN, and formats with ^DF (templates), are skipped rather than
// decoded. The input is an untrusted upload, so FuzzZPLPreview runs this
// too.

type zplTrackingEntry struct {
	Carrier string `json:"carrier"`
	Value   string `json:"value"`
}

var (
	zplFormatRe  = regexp.MustCompile(`(?is)\^XA(.*?)(?:\^XZ|$)`)
	zplFieldEnd  = regexp.MustCompile(`(?i)\^FS`)
	zplBarcodeRe = regexp.MustCompile(`(?i)\^B[C3]`)
	zplFieldData = regexp.MustCompile(`(?i)\^FD`)
	zplSkipField = regexp.MustCompile(`(?i)\^F[HN]`)
	zplTemplate  = regexp.MustCompile(`(?i)\^DF`)
)

// zplTracking is each format's best tracking number by the classification
// rules, in file order, without repeats (copies of one label).
func zplTracking(data []byte) []zplTrackingEntry {
	var out []zplTrackingEntry
	seen := map[zplTrackingEntry]bool{}
	for _, m := range zplFormatRe.FindAllSubmatch(data, -1) {
		format := string(m[1])
		if zplTemplate.MatchString(format) {
			continue
		}
		var values []string
		for _, field := range zplFieldEnd.Split(format, -1) {
			if v, ok := zplBarcodeValue(field); ok {
				values = append(values, v)
			}
		}
		if t := bestTracking(values); t.rank > 0 {
			e := zplTrackingEntry{Carrier: t.carrier, Value: t.value}
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// zplBarcodeValue is what a ^BC or ^B3 field encodes: its ^FD data up to the
// next command, with Code 128's invocation codes applied.
func zplBarcodeValue(field string) (string, bool) {
	if zplSkipField.MatchString(field) {
		return "", false
	}
	locs := zplBarcodeRe.FindAllStringIndex(field, -1)
	if len(locs) == 0 {
		return "", false
	}
	cmd := locs[len(locs)-1]
	rest := field[cmd[1]:]
	fd := zplFieldData.FindStringIndex(rest)
	if fd == nil {
		return "", false
	}
	value := rest[fd[1]:]
	if i := strings.IndexByte(value, '^'); i >= 0 {
		value = value[:i]
	}
	if strings.EqualFold(field[cmd[0]:cmd[1]], "^BC") {
		value = zplCode128Data(value)
	}
	return value, value != ""
}

// zplCode128Data applies ^BC's invocation codes: the first >8 (FNC1) before
// any data is dropped, as it's what makes the field GS1 (like ]C1), and every
// later >8 is GS. Start, subset and the other codes (>9 >: >; >5 >6 >7 >0–>3
// and the rest) go. >> becomes >, as the spec has it. Strictly, ZPL writes a
// literal > as >0 (and >< is ^, >= is ~), but tracking numbers are letters
// and digits only, so dropping those can't change a match.
func zplCode128Data(fd string) string {
	var b strings.Builder
	data := false // a data character or an FNC1 so far
	for i := 0; i < len(fd); i++ {
		c := fd[i]
		if c != '>' {
			b.WriteByte(c)
			data = true
			continue
		}
		if i+1 == len(fd) {
			break // a lone > at the end
		}
		i++
		switch fd[i] {
		case '>':
			b.WriteByte('>')
			data = true
		case '8':
			if data {
				b.WriteString(gs)
			}
			data = true
		}
	}
	return b.String()
}

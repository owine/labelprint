package main

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// The test label: a fixed, server-side ZPL format for judging darkness and
// speed, sent like feed (maintenance.go). It holds the parts of a shipping
// label that suffer first when the head runs too hot or too fast: 2-dot
// PDF417 and MaxiCode modules, Code 128 at 2, 3 and 4 dots, a grid of 1-dot
// lines and a solid block.
//
// It's laid out for the selected media rather than fixed at 4×6: elements are
// packed in rows, in priority order, and whatever doesn't fit is left out, so
// a 2×1 still carries the PDF417 and the grid. Module sizes stay in dots
// whatever the dpi; that's what the test is about.

// tlRect is a box in dots.
type tlRect struct{ x, y, w, h int }

func (a tlRect) overlaps(b tlRect) bool {
	return a.x < b.x+b.w && b.x < a.x+a.w && a.y < b.y+b.h && b.y < a.y+a.h
}

// tlItem is one element: a caption above a symbol. w and h are the symbol's
// footprint, quiet zones included; zpl draws it with that footprint at x, y.
type tlItem struct {
	caption string
	w, h    int
	zpl     func(x, y int) string
}

type tlPlaced struct {
	tlItem
	rect     tlRect // caption and symbol together
	sx, sy   int    // the symbol's footprint origin
	capH     int
	isHeader bool
}

func mmToDots(mm float64, dpi int) int { return int(math.Round(mm / 25.4 * float64(dpi))) }

// scaled turns a size chosen at 203 dpi into dots at dpi, so text and spacing
// keep their physical size. Module sizes don't go through this.
func scaled(v, dpi int) int { return max(1, (v*dpi+101)/203) }

func testLabelMargin(dpi int) int { return scaled(16, dpi) }

// textWidth estimates ^A0 text h dots high (width = height). Font 0 is a
// condensed face, about 0.45 em a character in the preview; half an em leaves
// some slack for the printer's own font.
func textWidth(s string, h int) int { return (len(s)*h + 1) / 2 }

const (
	testLabelGridCells = 8  // the grid is 8×8 cells…
	testLabelGridPitch = 10 // …of 10 dots, drawn in 1-dot lines
	testLabelCode128   = "123456789012"
	testLabelMaxiCode  = "001840000000000[)>_1E01_1D96LABELPRINTTEST0_1D_1E_04"
	// Code 128 subset C (mode A picks it for an even run of digits): start,
	// six digit pairs, check, then the 13-module stop.
	testLabelCode128Modules = 11 + 6*11 + 11 + 13
	testLabelPDF417Cols     = 3
	testLabelPDF417Rows     = 10
	testLabelPDF417RowH     = 6 // dots: 3× the 2-dot module
)

func testLabelItems(dpi int) []tlItem {
	side := testLabelGridCells*testLabelGridPitch + 1
	grid := func(x, y int) string {
		var b strings.Builder
		for i := 0; i <= testLabelGridCells; i++ {
			fmt.Fprintf(&b, "^FO%d,%d^GB1,%d,1^FS", x+i*testLabelGridPitch, y, side)
		}
		b.WriteByte('\n')
		for i := 0; i <= testLabelGridCells; i++ {
			fmt.Fprintf(&b, "^FO%d,%d^GB%d,1,1^FS", x, y+i*testLabelGridPitch, side)
		}
		return b.String()
	}
	// PDF417 is 17 modules a column plus 69 for start, stop and the row
	// indicators; 2 modules of quiet zone either side.
	pdfW := (17*testLabelPDF417Cols + 69 + 4) * 2
	pdf := func(x, y int) string {
		return fmt.Sprintf("^FO%d,%d^BY2^B7N,%d,2,%d,%d,N^FDLABELPRINT TEST^FS",
			x+4, y, testLabelPDF417RowH, testLabelPDF417Cols, testLabelPDF417Rows)
	}
	barH := scaled(50, dpi)
	code128 := func(module int) tlItem {
		quiet := 10 * module
		return tlItem{
			caption: fmt.Sprintf("code 128, %d dot module", module),
			w:       testLabelCode128Modules*module + 2*quiet, h: barH,
			zpl: func(x, y int) string {
				return fmt.Sprintf("^FO%d,%d^BY%d^BCN,%d,N,N,N,A^FD%s^FS", x+quiet, y, module, barH, testLabelCode128)
			},
		}
	}
	// MaxiCode has a fixed physical size, about 1.11 × 1.05 in; pad it a
	// little, since printers and previews place it slightly differently.
	// Mode 2 with a carrier-style message, as on shipping labels (zebrash
	// draws only that shape): service class 001, country 840, an all-zero
	// postcode and a dummy tracking number.
	pad := scaled(8, dpi)
	maxi := tlItem{
		caption: "maxicode",
		w:       int(math.Ceil(1.11*float64(dpi))) + 2*pad, h: int(math.Ceil(1.054*float64(dpi))) + 2*pad,
		zpl: func(x, y int) string {
			return fmt.Sprintf("^FO%d,%d^BD2,1,1^FH^FD%s^FS", x+pad, y+pad, testLabelMaxiCode)
		},
	}
	return []tlItem{
		{caption: "pdf417, 2 dot module", w: pdfW, h: testLabelPDF417Rows * testLabelPDF417RowH, zpl: pdf},
		{caption: "1 dot grid", w: side, h: side, zpl: grid},
		{caption: "solid block", w: side, h: side, zpl: func(x, y int) string {
			return fmt.Sprintf("^FO%d,%d^GB%d,%d,%d^FS", x, y, side, side, side)
		}},
		code128(2), code128(3), code128(4),
		maxi,
	}
}

// layoutTestLabel packs the header and then the items, in order, into rows on
// a pw × ll dot label. An item that fits neither the current row nor a new
// one is left out, and the next (maybe smaller) one tried. The header's second
// line, the size, goes too if that makes room for more of the test.
func layoutTestLabel(pw, ll, dpi int) []tlPlaced {
	full, short := packTestLabel(pw, ll, dpi, true), packTestLabel(pw, ll, dpi, false)
	if symbolCount(short) > symbolCount(full) {
		return short
	}
	return full
}

func symbolCount(placed []tlPlaced) int {
	n := 0
	for _, p := range placed {
		if !p.isHeader {
			n++
		}
	}
	return n
}

func packTestLabel(pw, ll, dpi int, withSize bool) []tlPlaced {
	margin, gap, rowGap := testLabelMargin(dpi), scaled(20, dpi), scaled(14, dpi)
	titleH, capH, capGap := scaled(30, dpi), scaled(20, dpi), scaled(4, dpi)
	right, bottom := pw-margin, ll-margin

	type line struct {
		text string
		h    int
	}
	header := []line{{"labelprint test", titleH}}
	if withSize {
		header = append(header, line{fmt.Sprintf("%d x %d dots, %d dpi", pw, ll, dpi), capH})
	}
	var out []tlPlaced
	y := margin
	for _, l := range header {
		w := textWidth(l.text, l.h)
		if margin+w > right || y+l.h > bottom {
			continue
		}
		out = append(out, tlPlaced{tlItem: tlItem{caption: l.text}, rect: tlRect{margin, y, w, l.h}, capH: l.h, isHeader: true})
		y += l.h + capGap
	}
	y += rowGap - capGap

	x, rowH := margin, 0
	for _, it := range testLabelItems(dpi) {
		w, h := max(it.w, textWidth(it.caption, capH)), capH+capGap+it.h
		switch {
		case x+w <= right && y+h <= bottom:
		case x > margin && margin+w <= right && y+rowH+rowGap+h <= bottom:
			x, y, rowH = margin, y+rowH+rowGap, 0
		default:
			continue
		}
		out = append(out, tlPlaced{tlItem: it, rect: tlRect{x, y, w, h}, sx: x, sy: y + capH + capGap, capH: capH})
		x += w + gap
		rowH = max(rowH, h)
	}
	return out
}

// testLabelZPL is the test label for media at dpi.
func testLabelZPL(media labelSize, dpi int) (string, error) {
	pw := mmToDots(clamp(media.WidthMm, 1, maxPreviewWidthMm), dpi)
	ll := mmToDots(clamp(media.HeightMm, 1, maxPreviewHeightMm), dpi)
	placed := layoutTestLabel(pw, ll, dpi)
	var b strings.Builder
	b.WriteString("^XA\n")
	// ^FX runs to the next caret or tilde, so the comment can't name the
	// commands with theirs.
	b.WriteString("^FX labelprint test label. It sets no darkness (SD, MD) or speed (PR) on purpose:" +
		" it prints at the settings LPrint and the printer already use.^FS\n")
	fmt.Fprintf(&b, "^PW%d^LL%d^LH0,0\n", pw, ll)
	for _, p := range placed {
		fmt.Fprintf(&b, "^FO%d,%d^A0N,%d,%d^FD%s^FS\n", p.rect.x, p.rect.y, p.capH, p.capH, p.caption)
		if p.isHeader {
			continue
		}
		b.WriteString(p.zpl(p.sx, p.sy))
		b.WriteByte('\n')
	}
	b.WriteString("^XZ\n")
	if symbolCount(placed) == 0 {
		return "", errors.New("this label size is too small for the test label")
	}
	return b.String(), nil
}

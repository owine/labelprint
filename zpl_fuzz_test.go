package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// FuzzZPLPreview feeds arbitrary bytes to the zpl preview, the path every
// uploaded .zpl takes. Whatever the input, render must hand back a result or
// an error in time and never take the process down: zebrash and the fonts and
// barcode libraries under it are fed untrusted uploads and have no recover of
// their own, so zpl.go's recover and timeout are what stand between a bad
// label and a crashed server. A panic escaping render crashes this test, which
// is the failure it exists to catch, so nothing here recovers. The preview's
// ZPL tracking parser (zplTracking) runs on every input too.
//
// Plain `go test` runs the seeds below as regression tests. To fuzz:
//
//	go test -run '^$' -fuzz '^FuzzZPLPreview$' -fuzztime 60s .
func FuzzZPLPreview(f *testing.F) {
	sample, err := os.ReadFile("testdata/sample.zpl")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(sample)
	for _, s := range zplFuzzSeeds() {
		f.Add([]byte(s))
	}

	// One renderer for every iteration in this process, as in the server. A
	// render that times out is abandoned, not stopped: its goroutine runs on
	// and keeps its slot. Sharing the renderer means its two slots cap those
	// goroutines at two however many inputs time out; a later iteration then
	// waits for a slot only until its deadline and gets that error back, which
	// still passes. A fresh renderer per input would let them pile up.
	z := newZPLRenderer(203)
	const grace = 5 * time.Second
	maxW := int(math.Ceil(maxPreviewWidthMm * float64(z.dpmm)))
	maxH := int(math.Ceil(maxPreviewHeightMm * float64(z.dpmm)))

	f.Fuzz(func(t *testing.T, data []byte) {
		// the preview reads tracking numbers from the same untrusted bytes
		for _, e := range zplTracking(data) {
			if e.Value == "" || (e.Carrier != "UPS" && e.Carrier != "USPS" && e.Carrier != "FedEx") {
				t.Fatalf("tracking entry %+v", e)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), z.timeout+grace)
		defer cancel()
		type result struct {
			p   *zplPreview
			err error
		}
		done := make(chan result, 1)
		start := time.Now()
		go func() {
			p, err := z.render(ctx, data, size4x6, maxPreviewPages)
			done <- result{p, err}
		}()
		var r result
		select {
		case r = <-done:
		case <-time.After(z.timeout + 2*grace):
			t.Fatalf("render still running after %s: past its own %s timeout and the caller's deadline", time.Since(start).Round(time.Second), z.timeout)
		}
		if r.err != nil {
			return // a refused label is fine: the printer reads the zpl itself
		}
		p := r.p
		if p == nil {
			t.Fatal("render returned neither a preview nor an error")
		}
		if len(p.Pages) > maxPreviewPages || len(p.Pages) > p.Labels {
			t.Fatalf("%d pages for %d labels, limit %d", len(p.Pages), p.Labels, maxPreviewPages)
		}
		wantW := int(math.Ceil(p.Size.WidthMm * float64(z.dpmm)))
		wantH := int(math.Ceil(p.Size.HeightMm * float64(z.dpmm)))
		if wantW > maxW || wantH > maxH {
			t.Fatalf("canvas %v×%v mm is past the %v×%v mm clamp", p.Size.WidthMm, p.Size.HeightMm, maxPreviewWidthMm, maxPreviewHeightMm)
		}
		for i, pg := range p.Pages {
			if pg.Page != i+1 {
				t.Fatalf("page %d numbered %d", i+1, pg.Page)
			}
			// zebrash narrows the image to a label's own ^PW, never widens it.
			b := decodePNG(t, pg.PNG).Bounds()
			if b.Dx() < 1 || b.Dx() > wantW || b.Dy() != wantH {
				t.Fatalf("page %d is %d×%d px, want up to %d wide and %d high", pg.Page, b.Dx(), b.Dy(), wantW, wantH)
			}
		}
	})
}

// Labels known to panic inside zebrash's dependencies. render must turn each
// into an error. If an update fixes a panic, an error is still the right
// answer: neither label can be drawn.
func TestZPLRenderContainsDependencyPanics(t *testing.T) {
	cases := map[string]string{
		// golang/freetype: a font stored with ~DU that parses but has no
		// glyph data, recalled with ^A@, indexes out of range on the first glyph
		"freetype": zplWithBrokenFont(),
		// ingridhq/maxicode: mode 6 data that passes zebrash's ^BD length
		// checks, then overruns the secondary message (found by this fuzzer)
		"maxicode": zplMaxiCodeOverrun,
	}
	z := newZPLRenderer(203)
	for name, zpl := range cases {
		if p, err := z.render(context.Background(), []byte(zpl), size4x6, 1); err == nil {
			t.Errorf("%s: previewed (%d pages); want an error", name, len(p.Pages))
		}
	}
}

const zplMaxiCodeOverrun = "^XA^FO50,50^BD6^FD001084000000\xf8[)>\x1e01\x1d96" +
	"00000000000000000000000000000000000000000000000000000000000000000000000000" +
	"qqq000000000000000000000000000000000000^FS^XZ"

// zplWithBrokenFont stores brokenTTF on R: and draws text in it, both ways a
// label can name a stored font: ^A@ by path and ^CW by alias.
func zplWithBrokenFont() string {
	ttf := brokenTTF()
	return fmt.Sprintf("^XA~DUR:FUZZ.TTF,%d,%s^CWZ,R:FUZZ.TTF"+
		"^FO50,50^A@N,40,40,R:FUZZ.TTF^FDAb9^FS^FO50,120^AZN,30,30^FDx^FS^XZ",
		len(ttf), strings.ToUpper(hex.EncodeToString(ttf)))
}

// brokenTTF is about the smallest font freetype's Parse accepts: one glyph, a
// one-segment format 4 cmap, and no loca or glyf table. Loading any glyph
// reads the missing loca and indexes out of range. (The prior find was a
// fuzzed real font; this is the same failure in 254 synthetic bytes.)
func brokenTTF() []byte {
	be := binary.BigEndian
	head := make([]byte, 54)
	be.PutUint16(head[18:], 1000) // unitsPerEm; indexToLocFormat 0 at 50
	maxp := make([]byte, 32)
	be.PutUint16(maxp[4:], 1) // numGlyphs
	hhea := make([]byte, 36)
	be.PutUint16(hhea[34:], 1) // numberOfHMetrics
	hmtx := make([]byte, 4)    // 4*nHMetric + 2*(nGlyph-nHMetric)
	cmap := []byte{
		0, 0, 0, 1, // version, one subtable
		0, 0, 0, 3, 0, 0, 0, 12, // Unicode BMP, at offset 12
		0, 4, 0, 24, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, // format 4, length 24, one segment
		0xff, 0xff, 0, 0, 0xff, 0xff, 0, 1, 0, 0, // end, pad, start, delta, range offset
	}
	tables := []struct {
		tag  string
		data []byte
	}{{"cmap", cmap}, {"head", head}, {"hhea", hhea}, {"hmtx", hmtx}, {"maxp", maxp}}

	dirLen := 12 + 16*len(tables)
	dir := make([]byte, 12, dirLen)
	be.PutUint32(dir, 0x00010000)
	be.PutUint16(dir[4:], uint16(len(tables)))
	var body []byte
	for _, tb := range tables {
		rec := make([]byte, 16)
		copy(rec, tb.tag)
		be.PutUint32(rec[8:], uint32(dirLen+len(body)))
		be.PutUint32(rec[12:], uint32(len(tb.data)))
		dir = append(dir, rec...)
		body = append(body, tb.data...)
	}
	return append(dir, body...)
}

// z64 encodes data the way ^GF and ~DG accept it: :Z64:base64(zlib):crc.
func z64(data []byte) string {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	_, _ = w.Write(data)
	_ = w.Close()
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	return fmt.Sprintf(":Z64:%s:%04X", b64, crc32.ChecksumIEEE([]byte(b64))&0xffff)
}

// zplFuzzSeeds are synthetic: no real names or addresses, and small enough to
// keep plain `go test` quick.
func zplFuzzSeeds() []string {
	bitmap := bytes.Repeat([]byte{0xf0, 0x0f}, 32) // 64 bytes, 4 bytes a row
	return []string{
		// a font freetype panics on, by path and by alias
		zplWithBrokenFont(),
		"^XA~DUR:FUZZ.TTF,8,0001000000000000^FO10,10^A@N,30,30,R:FUZZ.TTF^FDx^FS^XZ",
		"^XA~DUE:X.TTF,99999999,00^XZ",

		// MaxiCode: modes, bad postcodes, empty, overlong, binary
		"^XA^FO40,40^BD2^FD001840000000000000[)>_1E01_1D96X_1D_1E_04^FS^XZ",
		"^XA^FO40,40^BD2^FH^FD001840123450000[)>_1E01_1D96TEST_1D_1E_04^FS^XZ",
		"^XA^FO50,50^BD6^FD0010840000000[)>\x1e01\x1d96" + strings.Repeat("0", 110) + "^FS^XZ",
		"^XA^FO50,50^BD6^FD0000000000000\xbfm\x16\x9d\x83\xbf\x06" + strings.Repeat("0", 100) + "\x00" + strings.Repeat("0", 22) + "^FS^XZ",
		"^XA^FO50,50^BD3^FDZZZZZZZZZZZZZZZ[)>\x1e01\x1d96^FS^XZ",
		"^XA^FO50,50^BD4,9,9^FD" + strings.Repeat("HELLO WORLD 123 ", 12) + "^FS^XZ",
		zplMaxiCodeOverrun, // panics in maxicode
		"^XA^BD^FD^FS^XZ",
		"^XA^BD9^FD\xff\xfe\x00^FS^XZ",

		// ^GF: huge, negative, zero, mismatched, compressed, Z64 and B64
		"^XA^FO10,10^GFA,64,64,4," + strings.ToUpper(hex.EncodeToString(bitmap)) + "^FS^XZ",
		"^XA^FO10,10^GFA,-1,-1,-1,FF^FS^XZ",
		"^XA^FO10,10^GFA,0,0,0,^FS^XZ",
		"^XA^FO10,10^GFA,99999999,99999999,99999,FF^FS^XZ",
		"^XA^FO10,10^GFA,8,99999,1,FF^FS^XZ",
		"^XA^FO10,10^GFA,400,400,10,zzF,!:^FS^XZ",
		"^XA^FO10,10^GFB,8,8,1,\xff\x00\xff\x00\xff\x00\xff\x00^FS^XZ",
		"^XA^FO10,10^GFA,64,64,4," + z64(bitmap) + "^FS^XZ",
		"^XA^FO10,10^GFA,64,64,4,:Z64:!!!!:0000^FS^XZ",
		"^XA^FO10,10^GFA,64,64,4," + strings.Replace(z64(bitmap), ":Z64:", ":B64:", 1) + "^FS^XZ",
		"^XA^FO10,10^GFA,9999999,9999999,99," + z64(make([]byte, 4096)) + "^FS^XZ",
		"~DGR:G.GRF,64,4," + strings.ToUpper(hex.EncodeToString(bitmap)) + "^XA^FO10,10^XGR:G.GRF,9999,9999^FS^XZ",

		// ^PW / ^LL extremes and the canvas clamp
		"^XA^PW1^LL1^FO0,0^GB10,10,10^FS^XZ",
		"^XA^PW99999999999999999999^LL-5^FO0,0^GB10,10,10^FS^XZ",
		"^XA^PW0^LL0^XZ^XA^PW99999^LL99999999^FO0,0^GB960,8000,4^FS^XZ", // the largest canvas

		// deep repetition
		"^XA" + strings.Repeat("^FO10,10^FDx", 2000) + "^FS^XZ",
		"^XA^FD" + strings.Repeat("^FD", 3000) + "^FS^XZ",
		"^XA^FO10,10^FB800,9999,0,L^FD" + strings.Repeat("x\\&", 1000) + "^FS^XZ",
		strings.Repeat("^XA^FO10,10^GB50,30,30^FS^XZ", maxPreviewPages+1) + "^XA^PW100^LL50^XZ", // past the page limit, small canvas

		// truncated mid-command
		"^XA^FO", "^XA^FO10,10^GFA,10,10,", "^XA^BD", "^XA^A@N,40,40", "^XA^FD",
		"~DUR:X.TTF,100,0001", ":Z64:", "^", "~", "^XA^BCN,100,Y,N,N^FD>:",
	}
}

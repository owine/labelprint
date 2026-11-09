package main

import (
	"image"
	"testing"
	"time"
)

// FuzzBarcodeDecode feeds small arbitrary gray images to the barcode check,
// the path every previewed label takes. Whatever the pixels, checkGray must
// answer a known status in time and never take the process down. As in
// FuzzZPLPreview, a panic inside gozxing that checkGray recovers is a pass
// (it's "error"); one that escapes, or a decode that never returns, fails.
// The image is w×h (each up to 96), its pixels pix repeated, or white.
//
// Plain `go test` runs the seeds below as regression tests. To fuzz:
//
//	go test -run '^$' -fuzz '^FuzzBarcodeDecode$' -fuzztime 30s .
func FuzzBarcodeDecode(f *testing.F) {
	bars := code128(f, "1234")
	row := make([]byte, len(bars))
	for i, b := range bars {
		row[i] = 255
		if b {
			row[i] = 0
		}
	}
	f.Add(uint8(len(row)), uint8(20), row) // a readable Code 128, every row the same
	f.Add(uint8(0), uint8(0), []byte{})
	f.Add(uint8(1), uint8(1), []byte{0})
	f.Add(uint8(8), uint8(8), []byte{0, 255})
	f.Add(uint8(96), uint8(96), []byte{0, 0, 255, 255, 0, 255, 128})
	f.Add(uint8(33), uint8(96), []byte{0, 255, 255, 0, 0, 0, 255})

	valid := map[string]bool{checkOK: true, checkReads: true, checkNone: true, checkTimedOut: true, checkError: true}
	f.Fuzz(func(t *testing.T, w, h uint8, pix []byte) {
		g := image.NewGray(image.Rect(0, 0, int(w)%97, int(h)%97))
		for i := range g.Pix {
			g.Pix[i] = 255
			if len(pix) > 0 {
				g.Pix[i] = pix[i%len(pix)]
			}
		}
		done := make(chan labelCheck, 1)
		go func() { done <- checkGray(g, decodeLabel) }()
		select {
		case c := <-done:
			if !valid[c.Status] {
				t.Fatalf("status %q", c.Status)
			}
			if (c.Status == checkOK) != (c.Tracking != "") {
				t.Fatalf("status %q with tracking %q", c.Status, c.Tracking)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("a %d×%d image still decoding after 10 s", g.Rect.Dx(), g.Rect.Dy())
		}
	})
}

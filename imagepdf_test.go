package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"runtime"
	"slices"
	"testing"
	"time"
)

// ---------------------------------------------------------------- synthetic images (never real labels)

func encodePNG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeGIF(t testing.TB, g *gif.GIF) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// bigPNG is a w × h PNG whose every byte of pixel data is v: built by hand,
// with Adam7 passes when interlaced (Go's encoder can't), and quick to make
// at any size. colorType 3 gets a two-entry palette.
func bigPNG(t testing.TB, w, h int, depth, colorType, interlace byte, v byte) []byte {
	t.Helper()
	channels := map[byte]int{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[colorType]
	bpp := channels * int(depth) / 8
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestSpeed)
	rows := func(pw, ph int) {
		if pw == 0 || ph == 0 {
			return
		}
		row := bytes.Repeat([]byte{v}, 1+pw*bpp)
		row[0] = 0 // filter: none
		for range ph {
			_, _ = zw.Write(row)
		}
	}
	if interlace == 0 {
		rows(w, h)
	} else {
		for _, p := range [][4]int{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}} {
			rows((w-p[0]+p[2]-1)/p[2], (h-p[1]+p[3]-1)/p[3])
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var extra [][]byte
	if colorType == 3 {
		extra = append(extra, pngChunk("PLTE", []byte{255, 255, 255, 0, 0, 0}))
	}
	extra = append(extra, pngChunk("IDAT", z.Bytes()), pngChunk("IEND", nil))
	return pngHeader(uint32(w), uint32(h), depth, colorType, interlace, extra...)
}

// pngChunk is one PNG chunk: length, type, data, CRC.
func pngChunk(typ string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(append(out, typ...), data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

// pngHeader is a PNG's signature and IHDR, then any extra chunks, and nothing
// else: DecodeConfig reads it, and any decode fails. A paletted header needs
// a PLTE and one more chunk after it before DecodeConfig is satisfied.
func pngHeader(w, h uint32, depth, colorType, interlace byte, extra ...[]byte) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, depth, colorType, 0, 0, interlace)
	return slices.Concat(append([][]byte{[]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)}, extra...)...)
}

// physChunk is a pHYs chunk: x and y pixels per unit (1: metre).
func physChunk(x, y uint32, unit byte) []byte {
	p := binary.BigEndian.AppendUint32(nil, x)
	p = binary.BigEndian.AppendUint32(p, y)
	return pngChunk("pHYs", append(p, unit))
}

// withPHYs splices a pHYs chunk in after the IHDR: Go's encoder writes none.
func withPHYs(data []byte, ppm uint32) []byte {
	const ihdrEnd = 8 + 4 + 4 + 13 + 4
	return slices.Concat(data[:ihdrEnd], physChunk(ppm, ppm, 1), data[ihdrEnd:])
}

// withJFIF splices a JFIF APP0 in after the SOI: Go's encoder writes none.
func withJFIF(data []byte, units byte, x, y uint16) []byte {
	app0 := []byte{0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 2, units, byte(x >> 8), byte(x), byte(y >> 8), byte(y), 0, 0}
	return slices.Concat(data[:2], app0, data[2:])
}

// jpegScan is one scan of a hand-built JPEG: the components it codes (0-based)
// and its spectral selection and successive approximation.
type jpegScan struct {
	comps          []int
	ss, se, ah, al byte
}

// handJPEG builds the JPEGs Go's encoder can't write: CMYK and RGB (adobe:
// an Adobe APP14 with transform 0, for n 4 or 3), 4:4:4 YCbCr, and
// progressive with any scans, AC refinement included. Every coefficient is
// zero (one-symbol Huffman tables, all-zero entropy bytes), so it decodes to
// flat samples, and it's small at any size. A nil scans is one baseline scan
// of every component.
func handJPEG(w, h, n int, progressive, adobe bool, scans []jpegScan) []byte {
	seg := func(marker byte, p []byte) []byte {
		return append([]byte{0xFF, marker, byte((len(p) + 2) >> 8), byte(len(p) + 2)}, p...)
	}
	out := []byte{0xFF, 0xD8}
	if adobe {
		out = append(out, seg(0xEE, []byte{'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0})...)
	}
	out = append(out, seg(0xDB, append([]byte{0}, bytes.Repeat([]byte{1}, 64)...))...)
	sof := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(n)}
	for c := range n {
		sof = append(sof, byte(c+1), 0x11, 0)
	}
	marker := byte(0xC0)
	if progressive {
		marker = 0xC2
	}
	out = append(out, seg(marker, sof)...)
	one := append([]byte{1}, make([]byte, 15)...) // one code, of length 1
	dht := slices.Concat([]byte{0x00}, one, []byte{0}, []byte{0x10}, one, []byte{0})
	out = append(out, seg(0xC4, dht)...)
	if scans == nil {
		all := make([]int, n)
		for c := range all {
			all[c] = c
		}
		scans = []jpegScan{{comps: all, ss: 0, se: 63}}
	}
	blocks := ((w + 7) / 8) * ((h + 7) / 8)
	for _, s := range scans {
		sos := []byte{byte(len(s.comps))}
		for _, c := range s.comps {
			sos = append(sos, byte(c+1), 0x00)
		}
		sos = append(sos, s.ss, s.se, s.ah<<4|s.al)
		out = append(out, seg(0xDA, sos)...)
		bits := blocks * len(s.comps) // one symbol a block (progressive)
		if !progressive {
			bits *= 2 // DC 0, then EOB
		}
		out = append(out, make([]byte, bits/8+16)...)
	}
	return append(out, 0xFF, 0xD9)
}

// progScans is a progressive scan script for n components: DC for all, then
// per component an AC first pass and refine AC refinement passes (Ah=1).
func progScans(n, refine int) []jpegScan {
	all := make([]int, n)
	for c := range all {
		all[c] = c
	}
	s := []jpegScan{{comps: all}}
	for c := range n {
		s = append(s, jpegScan{comps: []int{c}, ss: 1, se: 63, al: 1})
		for range refine {
			s = append(s, jpegScan{comps: []int{c}, ss: 1, se: 63, ah: 1})
		}
	}
	return s
}

func wantUserError(t *testing.T, err error, status int, msg string) {
	t.Helper()
	var ue *userError
	if !errors.As(err, &ue) || ue.Status != status || ue.Msg != msg {
		t.Fatalf("error %v, want %d %q", err, status, msg)
	}
}

// ---------------------------------------------------------------- before decoding

func TestCheckImageSizes(t *testing.T) {
	// a zero-sized GIF: header, an empty logical screen, trailer
	gif0 := []byte("GIF89a\x00\x00\x00\x00\x00\x00\x00\x3b")
	// a zero-height JPEG: SOI and an SOF0 of 8 × 0
	jpg0 := handJPEG(8, 0, 1, false, false, nil)
	cases := []struct {
		name   string
		data   []byte
		kind   docKind
		status int
		msg    string
	}{
		{"not an image", []byte("\x89PNG\r\n\x1a\nnot really"), kindPNG, 415, "couldn't read that image"},
		{"zero-sized gif", gif0, kindGIF, 422, "that image is empty"},
		{"zero-sized jpeg", jpg0, kindJPEG, 422, "that image is empty"},
		// image/png refuses a zero size in DecodeConfig
		{"zero-sized png", pngHeader(0, 10, 8, 0, 0), kindPNG, 415, "couldn't read that image"},
		// header only: a decode would fail with a 415
		{"100000 square", pngHeader(100000, 100000, 8, 0, 0), kindPNG, 422, "that image is too large to print as a label"},
		{"4100 wide", encodePNG(t, image.NewGray(image.Rect(0, 0, 4100, 100))), kindPNG, 422, "that image is too large to print as a label"},
		// 16 M px × (8 + 1) B: over 96 MB
		{"16-bit 4000 square", pngHeader(4000, 4000, 16, 6, 0), kindPNG, 422, "that image is too large to print as a label"},
		{"progressive cmyk 4000 square", handJPEG(4000, 4000, 4, true, true, progScans(4, 0))[:300], kindJPEG, 422,
			"that image is too large to print as a label"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := checkImage(c.data, c.kind)
			wantUserError(t, err, c.status, c.msg)
		})
	}
	// what each class admits at the side limit: the estimate allows it
	for _, c := range []struct {
		name string
		data []byte
		kind docKind
	}{
		{"8-bit rgba 4000 square", pngHeader(4000, 4000, 8, 6, 0), kindPNG},
		{"paletted 4000 square", pngHeader(4000, 4000, 8, 3, 0, pngChunk("PLTE", []byte{0, 0, 0}), pngChunk("IDAT", nil)), kindPNG},
		{"gray jpeg 4000 square", handJPEG(4000, 4000, 1, false, false, nil)[:300], kindJPEG},
		{"progressive gray jpeg 4000 square", handJPEG(4000, 4000, 1, true, false, progScans(1, 0))[:300], kindJPEG},
	} {
		if w, h, err := checkImage(c.data, c.kind); err != nil || w != 4000 || h != 4000 {
			t.Errorf("%s: %d × %d, %v", c.name, w, h, err)
		}
	}
}

func TestDecodeBytesPerPixel(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		want float64
	}{
		{"paletted", pngHeader(10, 10, 8, 3, 0), 1},
		{"1-bit gray", pngHeader(10, 10, 1, 0, 0), 4},
		{"8-bit rgb", pngHeader(10, 10, 8, 2, 0), 4},
		{"8-bit rgba", pngHeader(10, 10, 8, 6, 0), 4},
		{"16-bit gray", pngHeader(10, 10, 16, 0, 0), 8},
		{"16-bit rgba, interlaced", pngHeader(10, 10, 16, 6, 1), 16},
		{"paletted, interlaced", pngHeader(10, 10, 8, 3, 1), 2},
	} {
		if got := pngBytesPerPixel(c.data); got != c.want {
			t.Errorf("png %s: %g B/px, want %g", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		name string
		data []byte
		want float64
	}{
		{"gray", handJPEG(8, 8, 1, false, false, nil), 1},
		{"ycbcr", handJPEG(8, 8, 3, false, false, nil), 7},
		{"cmyk", handJPEG(8, 8, 4, false, true, nil), 8},
		{"progressive gray", handJPEG(8, 8, 1, true, false, progScans(1, 0)), 1 + 4},
		{"progressive ycbcr", handJPEG(8, 8, 3, true, false, progScans(3, 0)), 7 + 12},
		{"progressive cmyk", handJPEG(8, 8, 4, true, true, progScans(4, 0)), 8 + 16},
		{"no sof", []byte{0xFF, 0xD8, 0xFF, 0xD9}, 8},
	} {
		if got := scanJPEG(c.data).bytesPerPixel(); got != c.want {
			t.Errorf("jpeg %s: %g B/px, want %g", c.name, got, c.want)
		}
	}
}

func TestScanJPEGCountsEverySOS(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		want int
	}{
		{"go's encoder", encodeJPEG(t, image.NewGray(image.Rect(0, 0, 16, 16))), 1},
		{"fill bytes", []byte{0xFF, 0xFF, 0xFF, 0xDA, 0, 0, 0xFF, 0xDA}, 2},
		{"stuffed and restart", []byte{0xFF, 0x00, 0xDA, 0xFF, 0xD0, 0xDA, 0xDA}, 0},
		{"ff at the end", []byte{0xFF, 0xDA, 0xFF}, 1},
		{"64 scans", handJPEG(8, 8, 1, true, false, progScans(1, 62)), 64},
	} {
		if got := scanJPEG(c.data).scans; got != c.want {
			t.Errorf("%s: %d scans, want %d", c.name, got, c.want)
		}
	}
}

// Go's decoder walks every block on every scan and never checks a context,
// so the scan count is all that bounds a JPEG's decode time.
func TestJPEGScanLimit(t *testing.T) {
	ok := handJPEG(64, 64, 1, true, false, progScans(1, 62)) // 64 scans, AC refinement included
	if _, _, err := checkImage(ok, kindJPEG); err != nil {
		t.Fatalf("64 scans: %v", err)
	}
	if img, _, err := image.Decode(bytes.NewReader(ok)); err != nil || img.Bounds().Dx() != 64 {
		t.Fatalf("64 scans don't decode: %v", err)
	}
	// 65 scans at 4000 square: a decode would take seconds, or fail on the
	// missing entropy data.
	big := handJPEG(4000, 4000, 1, true, false, progScans(1, 63))
	big = big[:bytes.LastIndex(big, []byte{0xFF, 0xDA})+16]
	start := time.Now()
	_, _, err := checkImage(big, kindJPEG)
	wantUserError(t, err, 415, "couldn't read that image")
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("refusing took %s: it must not decode", d)
	}
}

// ---------------------------------------------------------------- decoding

// flattenRef is flatten through At, the slow way.
func flattenRef(img image.Image, w, h int) *image.Gray {
	g := newWhite(w, h)
	r := img.Bounds().Intersect(g.Rect)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			g.Pix[y*g.Stride+x] = onWhite(img.At(x, y).RGBA())
		}
	}
	return g
}

type setter interface{ Set(x, y int, c color.Color) }

// testImages is one 100 × 100 image of every type the decoders return, with
// a gradient, a black block and (where there's alpha) a see-through half.
func testImages() map[string]image.Image {
	r := image.Rect(0, 0, 100, 100)
	pal := color.Palette{color.White, color.Black, color.RGBA{0x80, 0x40, 0x20, 0xff}, color.Transparent}
	imgs := map[string]image.Image{
		"gray": image.NewGray(r), "gray16": image.NewGray16(r), "rgba": image.NewRGBA(r),
		"nrgba": image.NewNRGBA(r), "rgba64": image.NewRGBA64(r), "nrgba64": image.NewNRGBA64(r),
		"cmyk": image.NewCMYK(r), "paletted": image.NewPaletted(r, pal),
		"ycbcr420": image.NewYCbCr(r, image.YCbCrSubsampleRatio420),
		"ycbcr444": image.NewYCbCr(r, image.YCbCrSubsampleRatio444),
		"alpha":    image.NewAlpha(r), // no decoder returns it: the At fallback
	}
	for _, img := range imgs {
		for y := range 100 {
			for x := range 100 {
				v := uint8(x * 255 / 99)
				c := color.Color(color.NRGBA{v, uint8(255 - v), uint8(y * 2), 255})
				if y >= 50 {
					c = color.NRGBA{v, v, v, uint8(x * 2)} // see-through, more so to the left
				}
				if x >= 40 && x < 60 && y >= 10 && y < 30 {
					c = color.Black
				}
				switch m := img.(type) {
				case *image.YCbCr:
					rr, gg, bb, _ := c.RGBA()
					yy, cb, cr := color.RGBToYCbCr(uint8(rr>>8), uint8(gg>>8), uint8(bb>>8))
					m.Y[m.YOffset(x, y)], m.Cb[m.COffset(x, y)], m.Cr[m.COffset(x, y)] = yy, cb, cr
				case *image.Paletted:
					m.SetColorIndex(x, y, uint8((x/25+y/25)%4))
				case setter:
					m.Set(x, y, c)
				}
			}
		}
	}
	return imgs
}

func TestFlattenMatchesAt(t *testing.T) {
	for name, img := range testImages() {
		got, want := flatten(img, 100, 100), flattenRef(img, 100, 100)
		for i := range got.Pix {
			if d := int(got.Pix[i]) - int(want.Pix[i]); d < -1 || d > 1 {
				t.Fatalf("%s: pixel (%d,%d) = %d, want %d", name, i%100, i/100, got.Pix[i], want.Pix[i])
			}
		}
	}
	// a transparent pixel is white, never black
	n := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	n.Pix = []uint8{0, 0, 0, 0, 0, 0, 0, 255}
	if g := flatten(n, 2, 1); g.Pix[0] != 255 || g.Pix[1] != 0 {
		t.Errorf("transparent black = %d, opaque black = %d", g.Pix[0], g.Pix[1])
	}
}

// At allocates for most types, once a pixel: flatten mustn't.
func TestFlattenAllocsAreConstant(t *testing.T) {
	for name, img := range testImages() {
		if name == "alpha" {
			continue // the At fallback
		}
		if n := testing.AllocsPerRun(5, func() { flatten(img, 100, 100) }); n > 4 {
			t.Errorf("%s: %v allocations for 10000 pixels", name, n)
		}
	}
}

// A GIF frame can be smaller than its logical screen and offset within it;
// gif.Decode returns only the first frame.
func TestFlattenGIFFrame(t *testing.T) {
	pal := color.Palette{color.White, color.Black}
	frame := image.NewPaletted(image.Rect(10, 20, 30, 40), pal)
	for i := range frame.Pix {
		frame.Pix[i] = 1
	}
	second := image.NewPaletted(image.Rect(0, 0, 50, 60), pal) // all white
	data := encodeGIF(t, &gif.GIF{Image: []*image.Paletted{frame, second}, Delay: []int{0, 0},
		Config: image.Config{ColorModel: pal, Width: 50, Height: 60}})
	w, h, err := checkImage(data, kindGIF)
	if err != nil || w != 50 || h != 60 {
		t.Fatalf("%d × %d, %v", w, h, err)
	}
	g, err := decodeGray(data, w, h)
	if err != nil {
		t.Fatal(err)
	}
	for y := range 60 {
		for x := range 50 {
			want := uint8(255)
			if image.Pt(x, y).In(frame.Rect) {
				want = 0
			}
			if got := g.Pix[y*g.Stride+x]; got != want {
				t.Fatalf("(%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
}

// The decode-memory estimate holds: decoding the largest image each class
// admits, with the decoded image and the gray still held, grows the live
// heap by no more than the estimate (1 MB of slack for the runtime's own).
func TestDecodeLiveHeap(t *testing.T) {
	side := func(bpp float64) int { return min(maxImageSide, int(math.Sqrt(maxDecodeBytes/(bpp+1)))) }
	gifOf := func(s int) []byte {
		return encodeGIF(t, &gif.GIF{Image: []*image.Paletted{image.NewPaletted(image.Rect(0, 0, s, s),
			color.Palette{color.White, color.Black})}, Delay: []int{0}})
	}
	for _, c := range []struct {
		name string
		kind docKind
		bpp  float64
		make func(s int) []byte
	}{
		{"jpeg gray", kindJPEG, 1, func(s int) []byte { return handJPEG(s, s, 1, false, false, nil) }},
		{"jpeg ycbcr", kindJPEG, 7, func(s int) []byte { return handJPEG(s, s, 3, false, false, nil) }},
		{"jpeg rgb", kindJPEG, 7, func(s int) []byte { return handJPEG(s, s, 3, false, true, nil) }},
		{"jpeg cmyk", kindJPEG, 8, func(s int) []byte { return handJPEG(s, s, 4, false, true, nil) }},
		{"jpeg progressive gray", kindJPEG, 5, func(s int) []byte { return handJPEG(s, s, 1, true, false, progScans(1, 1)) }},
		{"jpeg progressive ycbcr", kindJPEG, 19, func(s int) []byte { return handJPEG(s, s, 3, true, false, progScans(3, 1)) }},
		{"jpeg progressive cmyk", kindJPEG, 24, func(s int) []byte { return handJPEG(s, s, 4, true, true, progScans(4, 1)) }},
		{"png paletted", kindPNG, 1, func(s int) []byte { return bigPNG(t, s, s, 8, 3, 0, 0) }},
		{"png gray", kindPNG, 4, func(s int) []byte { return bigPNG(t, s, s, 8, 0, 0, 0xff) }},
		{"png rgba", kindPNG, 4, func(s int) []byte { return bigPNG(t, s, s, 8, 6, 0, 0xff) }},
		{"png 16-bit rgba", kindPNG, 8, func(s int) []byte { return bigPNG(t, s, s, 16, 6, 0, 0xff) }},
		{"png interlaced rgba", kindPNG, 8, func(s int) []byte { return bigPNG(t, s, s, 8, 6, 1, 0xff) }},
		{"gif", kindGIF, 5, gifOf},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := side(c.bpp)
			data := c.make(s)
			w, h, err := checkImage(data, c.kind)
			if err != nil {
				t.Fatalf("%d square isn't admitted: %v", s, err)
			}
			if over := s + 1; over <= maxImageSide {
				if _, _, err := checkImage(c.make(over), c.kind); err == nil {
					t.Fatalf("%d square is admitted: the estimate isn't %g B/px", over, c.bpp)
				}
			}
			estimate := float64(w) * float64(h) * (c.bpp + 1)
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			img, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			g := flatten(img, w, h)
			runtime.GC()
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(img)
			runtime.KeepAlive(g)
			grew := float64(after.HeapAlloc) - float64(before.HeapAlloc)
			if grew > estimate+1<<20 {
				t.Errorf("%d square: live heap grew %.1f MB, over the %.1f MB estimate", s, grew/1e6, estimate/1e6)
			}
		})
	}
}

// ---------------------------------------------------------------- ppi

func TestImagePPI(t *testing.T) {
	m46, _ := mediaSize("na_index-4x6_4x6in")
	gray := func(w, h int) []byte { return encodePNG(t, image.NewGray(image.Rect(0, 0, w, h))) }
	jpg := encodeJPEG(t, image.NewGray(image.Rect(0, 0, 300, 300)))
	cases := []struct {
		name    string
		data    []byte
		kind    docKind
		w, h    int
		mediaOK bool
		ppi     float64
		src     string
	}{
		{"pHYs 7874 ppm", withPHYs(gray(800, 1200), 7874), kindPNG, 800, 1200, true, 200, "phys"},
		{"pHYs 11811 ppm", withPHYs(gray(300, 300), 11811), kindPNG, 300, 300, true, 300, "phys"},
		{"pHYs 3937 ppm is 100.0: in", withPHYs(gray(300, 300), 3937), kindPNG, 300, 300, false, 100, "phys"},
		{"pHYs 23622 ppm is 600.0: in", withPHYs(gray(300, 300), 23622), kindPNG, 300, 300, false, 600, "phys"},
		{"pHYs 23630 ppm is 600.2: out", withPHYs(gray(300, 300), 23630), kindPNG, 300, 300, false, 203, "default"},
		{"pHYs unit unknown", slices.Concat(gray(300, 300)[:33], physChunk(7874, 7874, 0), gray(300, 300)[33:]),
			kindPNG, 300, 300, false, 203, "default"},
		{"pHYs x and y disagree", slices.Concat(gray(300, 300)[:33], physChunk(7874, 8200, 1), gray(300, 300)[33:]),
			kindPNG, 300, 300, false, 203, "default"},
		{"pHYs 2835 ppm (72)", withPHYs(gray(300, 300), 2835), kindPNG, 300, 300, false, 203, "default"},
		{"JFIF 203 dpi", withJFIF(jpg, 1, 203, 203), kindJPEG, 300, 300, false, 203, "jfif"},
		{"JFIF 80 dots/cm", withJFIF(jpg, 2, 80, 80), kindJPEG, 300, 300, false, 203.2, "jfif"},
		{"JFIF 96", withJFIF(jpg, 1, 96, 96), kindJPEG, 300, 300, false, 203, "default"},
		{"JFIF aspect only", withJFIF(jpg, 0, 1, 1), kindJPEG, 300, 300, false, 203, "default"},
		{"800×1200 on 4×6", gray(800, 1200), kindPNG, 800, 1200, true, 200, "media"},
		{"812×1218", gray(812, 1218), kindPNG, 812, 1218, true, 203, "media"},
		{"1200×1800", gray(1200, 1800), kindPNG, 1200, 1800, true, 300, "media"},
		{"384×576", gray(384, 576), kindPNG, 384, 576, true, 96, "media"},
		{"1400×800 (a 1 in doc tab)", gray(1400, 800), kindPNG, 1400, 800, true, 200, "media+tab"},
		{"an odd aspect", gray(300, 300), kindPNG, 300, 300, true, 203, "default"},
		{"unknown media", gray(800, 1200), kindPNG, 800, 1200, false, 203, "default"},
		{"a match under 50 ppi", gray(160, 240), kindPNG, 160, 240, true, 203, "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ppi, src := imagePPI(c.data, c.kind, c.w, c.h, m46, c.mediaOK, 203)
			if math.Abs(ppi-c.ppi) > 1e-9 || src != c.src {
				t.Errorf("ppi %g (%s), want %g (%s)", ppi, src, c.ppi, c.src)
			}
		})
	}
}

func TestCheckImageSizeAtItsPPI(t *testing.T) {
	// 4000 px at 120 ppi is 33.3 in: about 45.8 Mpx at 203 dpi
	wantUserError(t, checkImageSize(4000, 4000, 120, 203), 422, "that image is too large to print as a label")
	wantUserError(t, checkImageSize(40, 40, 203, 203), 422, "that image is too small to print as a label")
	wantUserError(t, checkImageSize(800, 100, 203, 203), 422, "that image is too small to print as a label")
	for _, ok := range [][3]float64{{800, 1200, 200}, {102, 102, 203}, {3000, 3000, 203}} {
		if err := checkImageSize(int(ok[0]), int(ok[1]), ok[2], 203); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
}

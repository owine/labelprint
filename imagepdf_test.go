package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
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

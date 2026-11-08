package main

import (
	"bytes"
	"image"
	"image/draw"
	_ "image/jpeg" // pdftohtml writes DCT images out as JPEG
	"image/png"
	"os"
)

// toGray copies any decoded image into 8-bit gray with its origin at 0,0.
func toGray(img image.Image) *image.Gray {
	b := img.Bounds()
	if g, ok := img.(*image.Gray); ok && b.Min == (image.Point{}) {
		return g
	}
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(g, g.Rect, img, b.Min, draw.Src)
	return g
}

func newWhite(w, h int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for i := range g.Pix {
		g.Pix[i] = 255
	}
	return g
}

// threshold makes every pixel 0 (below t) or 255: what a 1-bit head prints,
// so LPrint's dither has nothing left to do.
func threshold(g *image.Gray, t uint8) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, g.Rect.Dx(), g.Rect.Dy()))
	for y := range out.Rect.Dy() {
		for x := range out.Rect.Dx() {
			if g.Pix[y*g.Stride+x] >= t {
				out.Pix[y*out.Stride+x] = 255
			}
		}
	}
	return out
}

// rotate turns g clockwise by quarter turns: source (x, y) of a w×h image goes
// to (h−1−y, x) for one turn. A pure rotation, never a mirror.
func rotate(g *image.Gray, turns int) *image.Gray {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	turns = ((turns % 4) + 4) % 4
	ow, oh := w, h
	if turns%2 == 1 {
		ow, oh = h, w
	}
	out := image.NewGray(image.Rect(0, 0, ow, oh))
	for y := range h {
		for x := range w {
			var ox, oy int
			switch turns {
			case 0:
				ox, oy = x, y
			case 1:
				ox, oy = h-1-y, x
			case 2:
				ox, oy = w-1-x, h-1-y
			case 3:
				ox, oy = y, w-1-x
			}
			out.Pix[oy*out.Stride+ox] = g.Pix[y*g.Stride+x]
		}
	}
	return out
}

// mirror flips g left to right.
func mirror(g *image.Gray) *image.Gray {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	out := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			out.Pix[y*out.Stride+w-1-x] = g.Pix[y*g.Stride+x]
		}
	}
	return out
}

// subGray copies r out of g, origin 0,0.
func subGray(g *image.Gray, r image.Rectangle) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Rect, g, r.Min, draw.Src)
	return out
}

// inkBounds is the bounding box of pixels darker than below within r.
func inkBounds(g *image.Gray, r image.Rectangle, below uint8) (image.Rectangle, bool) {
	r = r.Intersect(g.Rect)
	out, found := image.Rectangle{}, false
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if g.Pix[(y-g.Rect.Min.Y)*g.Stride+(x-g.Rect.Min.X)] < below {
				p := image.Rect(x, y, x+1, y+1)
				if !found {
					out, found = p, true
				} else {
					out = out.Union(p)
				}
			}
		}
	}
	return out, found
}

// isBilevel reports whether at least 99.5% of pixels have every channel at
// ≤ 15 or ≥ 240: a black/white bitmap, whatever its colour type.
func isBilevel(img image.Image) bool {
	b := img.Bounds()
	total, gray := b.Dx()*b.Dy(), 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			for _, c := range []uint32{r >> 8, g >> 8, bl >> 8} {
				if c > 15 && c < 240 {
					gray++
					break
				}
			}
		}
	}
	return total > 0 && gray*200 <= total
}

// scaleArea resizes g to w×h by averaging the source pixels under each
// destination pixel (nearest neighbour when enlarging).
func scaleArea(g *image.Gray, w, h int) *image.Gray {
	sw, sh := g.Rect.Dx(), g.Rect.Dy()
	out := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		y0 := y * sh / h
		y1 := max(y0+1, (y+1)*sh/h)
		for x := range w {
			x0 := x * sw / w
			x1 := max(x0+1, (x+1)*sw/w)
			sum, n := 0, 0
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					sum += int(g.Pix[sy*g.Stride+sx])
					n++
				}
			}
			out.Pix[y*out.Stride+x] = uint8(sum / n)
		}
	}
	return out
}

// centre places g in the middle of a white w×h canvas, cropping any overflow.
func centre(g *image.Gray, w, h int) *image.Gray {
	out := newWhite(w, h)
	ox, oy := (w-g.Rect.Dx())/2, (h-g.Rect.Dy())/2
	dst := image.Rect(ox, oy, ox+g.Rect.Dx(), oy+g.Rect.Dy()).Intersect(out.Rect)
	draw.Draw(out, dst, g, dst.Min.Sub(image.Pt(ox, oy)), draw.Src)
	return out
}

// encodeGray is an 8-bit grayscale PNG.
func encodeGray(g *image.Gray) ([]byte, error) {
	var b bytes.Buffer
	err := png.Encode(&b, g)
	return b.Bytes(), err
}

func decodePNGBytes(data []byte) (image.Image, error) { return png.Decode(bytes.NewReader(data)) }

// decodeImageFile reads a PNG or JPEG.
func decodeImageFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

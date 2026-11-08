package main

import (
	"image"
	"image/color"
	"slices"
	"testing"
)

func grayFrom(w, h int, v ...uint8) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	copy(g.Pix, v)
	return g
}

func TestRotateIsClockwiseAndNeverMirrors(t *testing.T) {
	src := grayFrom(2, 3, 1, 2, 3, 4, 5, 6) // rows: [1 2] [3 4] [5 6]
	cases := map[int][]uint8{
		0: {1, 2, 3, 4, 5, 6},
		1: {5, 3, 1, 6, 4, 2},
		2: {6, 5, 4, 3, 2, 1},
		3: {2, 4, 6, 1, 3, 5},
	}
	for turns, want := range cases {
		got := rotate(src, turns)
		if !slices.Equal(got.Pix, want) {
			t.Errorf("turns %d: %v, want %v", turns, got.Pix, want)
		}
	}
	if got := mirror(src); !slices.Equal(got.Pix, []uint8{2, 1, 4, 3, 6, 5}) {
		t.Errorf("mirror: %v", got.Pix)
	}
}

func TestThreshold(t *testing.T) {
	if got := threshold(grayFrom(3, 1, 0, 127, 128), 128); !slices.Equal(got.Pix, []uint8{0, 0, 255}) {
		t.Fatal(got.Pix)
	}
}

func TestCentre(t *testing.T) {
	got := centre(grayFrom(2, 2, 0, 0, 0, 0), 4, 4)
	want := []uint8{255, 255, 255, 255, 255, 0, 0, 255, 255, 0, 0, 255, 255, 255, 255, 255}
	if !slices.Equal(got.Pix, want) {
		t.Errorf("small onto big: %v", got.Pix)
	}
	big := grayFrom(3, 3, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	if got := centre(big, 1, 1); got.Pix[0] != 5 {
		t.Errorf("overflow should keep the middle, got %v", got.Pix)
	}
}

func TestScaleArea(t *testing.T) {
	g := grayFrom(4, 2, 0, 0, 255, 255, 0, 0, 255, 255)
	if got := scaleArea(g, 2, 1); !slices.Equal(got.Pix, []uint8{0, 255}) {
		t.Errorf("down: %v", got.Pix)
	}
	if got := scaleArea(grayFrom(1, 1, 7), 2, 2); !slices.Equal(got.Pix, []uint8{7, 7, 7, 7}) {
		t.Errorf("up: %v", got.Pix)
	}
}

func TestInkBounds(t *testing.T) {
	g := grayFrom(4, 3, 255, 255, 255, 255, 255, 0, 255, 255, 255, 255, 0, 255)
	r, ok := inkBounds(g, g.Rect, 128)
	if !ok || r != image.Rect(1, 1, 3, 3) {
		t.Fatalf("got %v %v", r, ok)
	}
	if _, ok := inkBounds(grayFrom(1, 1, 255), image.Rect(0, 0, 1, 1), 128); ok {
		t.Fatal("blank image has no ink")
	}
}

func TestIsBilevel(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	if !isBilevel(img) {
		t.Fatal("white is bilevel")
	}
	img.Set(1, 1, color.RGBA{128, 128, 128, 255}) // 1 of 10000: fine
	if !isBilevel(img) {
		t.Fatal("0.01% gray is still bilevel")
	}
	for x := range 100 {
		img.Set(x, 50, color.RGBA{128, 128, 128, 255}) // 1%
	}
	if isBilevel(img) {
		t.Fatal("1% gray isn't bilevel")
	}
}

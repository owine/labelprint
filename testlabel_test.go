package main

import (
	"context"
	"image/color"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var testLabelSizes = map[string]labelSize{
	"4x6":         size4x6,
	"4x4":         {101.6, 101.6},
	"3x2":         {76.2, 50.8},
	"2.25x1.25":   {57.15, 31.75},
	"2x1":         {50.8, 25.4},
	"4x6 300 dpi": size4x6, // the dpi is set per case below
}

func testLabelDPI(name string) int {
	if strings.HasSuffix(name, "300 dpi") {
		return 300
	}
	return 203
}

// The label is laid out for the media: ^PW/^LL match it in dots, every
// element sits inside the margin, and no two overlap.
func TestTestLabelLayout(t *testing.T) {
	for name, media := range testLabelSizes {
		t.Run(name, func(t *testing.T) {
			dpi := testLabelDPI(name)
			pw, ll := mmToDots(media.WidthMm, dpi), mmToDots(media.HeightMm, dpi)
			placed := layoutTestLabel(pw, ll, dpi)
			margin := testLabelMargin(dpi)
			for i, a := range placed {
				r := a.rect
				if r.x < margin || r.y < margin || r.x+r.w > pw-margin || r.y+r.h > ll-margin {
					t.Errorf("%q at %+v leaves the %dx%d label's %d-dot margin", a.caption, r, pw, ll, margin)
				}
				for _, b := range placed[i+1:] {
					if r.overlaps(b.rect) {
						t.Errorf("%q %+v overlaps %q %+v", a.caption, r, b.caption, b.rect)
					}
				}
			}
			zpl, err := testLabelZPL(media, dpi)
			if err != nil {
				t.Fatal(err)
			}
			if want := "^PW" + strconv.Itoa(pw) + "^LL" + strconv.Itoa(ll); !strings.Contains(zpl, want) {
				t.Errorf("no %s in\n%s", want, zpl)
			}
		})
	}
}

// A 4×6 label has room for everything the issue asks for.
func TestTestLabelContent4x6(t *testing.T) {
	zpl, err := testLabelZPL(size4x6, 203)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"^BY2^BCN", "^BY3^BCN", "^BY4^BCN", // Code 128 at 2, 3 and 4 dots
		"^BY2^B7N",       // PDF417 at 2 dots
		"^BD2,1,1^FH",    // MaxiCode
		"^GB1,81,1^FS",   // the grid's 1-dot lines
		"^GB81,81,81^FS", // the solid block
		"^FDlabelprint test^FS",
	} {
		if !strings.Contains(zpl, want) {
			t.Errorf("4x6 test label has no %q", want)
		}
	}
	if !strings.HasPrefix(zpl, "^XA") || !strings.HasSuffix(zpl, "^XZ\n") || strings.Count(zpl, "^XA") != 1 {
		t.Errorf("not one ^XA…^XZ format:\n%s", zpl)
	}
}

// The point is to see the darkness and speed LPrint and the printer already
// use, so the label never sets them, and it's always one copy (LPrint's ZPL
// driver ignores copies; ^PQ would be the only way).
func TestTestLabelLeavesSettingsAlone(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)[~^](SD|MD|PR|PQ|JC|JU|MN|MT|JS)`)
	for name, media := range testLabelSizes {
		zpl, err := testLabelZPL(media, testLabelDPI(name))
		if err != nil {
			t.Fatal(name, err)
		}
		if m := forbidden.FindString(zpl); m != "" {
			t.Errorf("%s: test label sends %s", name, m)
		}
		if !strings.Contains(zpl, "^FX") {
			t.Errorf("%s: no ^FX comment saying why", name)
		}
	}
}

// Small media drop what doesn't fit, most expendable first; too small for
// any of it is refused rather than printing a label with nothing to judge.
func TestTestLabelSmallMedia(t *testing.T) {
	zpl, err := testLabelZPL(labelSize{50.8, 25.4}, 203)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"^BY2^B7N", "^GB1,81,1^FS"} {
		if !strings.Contains(zpl, want) {
			t.Errorf("2x1 test label has no %q:\n%s", want, zpl)
		}
	}
	if strings.Contains(zpl, "^BD") || strings.Contains(zpl, "^BY4^BC") {
		t.Errorf("2x1 can't hold a maxicode or a 4-dot code 128:\n%s", zpl)
	}
	if _, err := testLabelZPL(labelSize{25.4, 12.7}, 203); err == nil {
		t.Error("1x0.5 in: want an error")
	}
}

// The app's own preview draws it without error, at the media size, inside the
// margin.
func TestTestLabelPreviews(t *testing.T) {
	z := newZPLRenderer(203)
	for _, name := range []string{"4x6", "4x4", "3x2", "2.25x1.25", "2x1"} {
		t.Run(name, func(t *testing.T) {
			media := testLabelSizes[name]
			zpl, err := testLabelZPL(media, 203)
			if err != nil {
				t.Fatal(err)
			}
			p, err := z.render(context.Background(), []byte(zpl), media, 1)
			if err != nil {
				t.Fatal(err)
			}
			if p.Labels != 1 || len(p.Pages) != 1 {
				t.Fatalf("labels=%d pages=%d", p.Labels, len(p.Pages))
			}
			if dir := os.Getenv("TEST_LABEL_PNG_DIR"); dir != "" {
				_ = os.WriteFile(dir+"/test-label-"+name+".png", p.Pages[0].PNG, 0o644)
			}
			img := decodePNG(t, p.Pages[0].PNG)
			b := img.Bounds()
			if pw, ll := mmToDots(media.WidthMm, 203), mmToDots(media.HeightMm, 203); b.Dx() != pw || b.Dy() != ll {
				t.Errorf("canvas %dx%d, want %dx%d", b.Dx(), b.Dy(), pw, ll)
			}
			// nothing in the outer 8 dots: nothing ran off the edge
			for y := b.Min.Y; y < b.Max.Y; y++ {
				for x := b.Min.X; x < b.Max.X; x++ {
					inner := x >= 8 && y >= 8 && x < b.Max.X-8 && y < b.Max.Y-8
					if !inner && color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y < 128 {
						t.Fatalf("ink at %d,%d, at the edge of the label", x, y)
					}
				}
			}
			if darkPixels(img) < b.Dx()*b.Dy()/50 {
				t.Errorf("hardly any ink: did anything draw?")
			}
		})
	}
}

// testdata/test-label-4x6.zpl is what a 4×6 test label sends, for reading in
// review. Regenerate with UPDATE_FIXTURES=1 go test -run Fixture.
func TestTestLabelFixture(t *testing.T) {
	zpl, err := testLabelZPL(size4x6, 203)
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/test-label-4x6.zpl"
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, []byte(zpl), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != zpl {
		t.Errorf("%s is stale; run UPDATE_FIXTURES=1 go test -run Fixture", path)
	}
}

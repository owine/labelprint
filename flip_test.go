package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// grayPNG decodes a label PNG to gray.
func grayPNG(t *testing.T, b []byte) *image.Gray {
	t.Helper()
	return toGray(decodePNG(t, b))
}

// assertTurned180 checks down is up turned 180°, pixel by pixel and without
// rotate: down(x, y) is up(w−1−x, h−1−y). The two being equal fails too, so a
// flip that does nothing can't pass.
func assertTurned180(t *testing.T, up, down *image.Gray) {
	t.Helper()
	if up.Rect != down.Rect {
		t.Fatalf("flipped label is %v, unflipped %v", down.Rect, up.Rect)
	}
	w, h := up.Rect.Dx(), up.Rect.Dy()
	for y := range h {
		for x := range w {
			if got, want := down.Pix[y*down.Stride+x], up.Pix[(h-1-y)*up.Stride+w-1-x]; got != want {
				t.Fatalf("flipped (%d,%d) = %d, want unflipped (%d,%d) = %d", x, y, got, w-1-x, h-1-y, want)
			}
		}
	}
	if bytes.Equal(up.Pix, down.Pix) {
		t.Fatal("the flipped label is the same as the unflipped one")
	}
}

// flip turns the finished label 180° on either path and changes nothing else:
// a native label stays native at its ppi, a rendered one keeps its scale.
// Both fixtures have a block in one corner, so neither is symmetric.
func TestLabelFlip(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, method string
		pdf          []byte
	}{
		{"native", "native", fixUPS2up(true, 200)}, // testdata/ups-2up.pdf
		{"rendered", "render", fixBand()},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := openTest(t, c.pdf)
			b := mustPlan(t, doc, ctx, []int{1}, opts46)[0].Boxes[0]
			up, err := doc.label(ctx, 1, b, false, opts46)
			if err != nil {
				t.Fatal(err)
			}
			down, err := doc.label(ctx, 1, b, true, opts46)
			if err != nil {
				t.Fatal(err)
			}
			if up.Method != c.method || down.Method != up.Method || down.PPI != up.PPI || down.Scale != up.Scale {
				t.Errorf("unflipped %s at %g ppi, scale %g; flipped %s at %g ppi, scale %g; want both %s, alike",
					up.Method, up.PPI, up.Scale, down.Method, down.PPI, down.Scale, c.method)
			}
			g := grayPNG(t, down.PNG)
			assertPureLabel(t, g, canvas46)
			assertTurned180(t, grayPNG(t, up.PNG), g)
		})
	}
}

// plan hands a crops page its flips; detected and whole-page plans have none.
// A doc tab printed separately is a box of its own, with its own flag.
func TestPlanFlips(t *testing.T) {
	ctx := context.Background()
	doc := openTest(t, makeLabelPDF(2))
	o := opts46
	// a label, its doc tab printed separately (both flipped), and a third box
	o.crops = map[int][]box{1: {{0, 0, 1, 0.5}, {0, 0.5, 0.5, 0.25}, {0, 0.75, 1, 0.25}}}
	o.flips = map[int][]bool{1: {true, true, false}}
	pl := mustPlan(t, doc, ctx, []int{1, 2}, o)
	if got := fmt.Sprint(pl[0].Flips); got != "[true true false]" || !pl[0].flip(0) || !pl[0].flip(1) || pl[0].flip(2) {
		t.Errorf("page 1 flips %s, want [true true false]", got)
	}
	if !pl[1].Auto || pl[1].Flips != nil || pl[1].flip(0) {
		t.Errorf("page 2 is detected: auto %v, flips %v; want no flips", pl[1].Auto, pl[1].Flips)
	}
	o.flips = nil
	if pl := mustPlan(t, doc, ctx, []int{1}, o); pl[0].Flips != nil || pl[0].flip(0) {
		t.Errorf("crops with nothing flipped: flips %v, want nil", pl[0].Flips)
	}
	o = opts46
	o.layout = "page"
	if pl := mustPlan(t, doc, ctx, []int{1}, o); pl[0].Flips != nil {
		t.Errorf("whole page: flips %v, want nil", pl[0].Flips)
	}
}

// previewLabels posts a preview and decodes its label images, with each
// one's method.
func previewLabels(t *testing.T, h http.Handler, name string, data []byte, fields map[string]string) ([]*image.Gray, []string) {
	t.Helper()
	rec, out := post(t, h, "/api/preview", name, data, fields)
	if rec.Code != 200 {
		t.Fatalf("preview: status %d: %s", rec.Code, rec.Body)
	}
	var gs []*image.Gray
	var methods []string
	for _, p := range out["pages"].([]any) {
		for _, li := range p.(map[string]any)["labelImages"].([]any) {
			m := li.(map[string]any)
			b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(m["image"].(string), "data:image/png;base64,"))
			if err != nil {
				t.Fatal(err)
			}
			gs = append(gs, grayPNG(t, b))
			methods = append(methods, m["method"].(string))
		}
	}
	return gs, methods
}

// cropsFrom is the crops field the page would send for a preview reply's
// boxes, with a fifth number, 1, on each box flip picks.
func cropsFrom(t *testing.T, out map[string]any, flip func(page, i int) bool) string {
	t.Helper()
	crops := map[string][][]float64{}
	for _, p := range out["pages"].([]any) {
		pg := p.(map[string]any)
		n := int(pg["page"].(float64))
		k := strconv.Itoa(n)
		for i, b := range pg["boxes"].([]any) {
			var a []float64
			for _, v := range b.([]any) {
				a = append(a, v.(float64))
			}
			if flip(n, i) {
				a = append(a, 1)
			}
			crops[k] = append(crops[k], a)
		}
	}
	j, err := json.Marshal(crops)
	if err != nil {
		t.Fatal(err)
	}
	return string(j)
}

// The preview shows a flipped box's label turned 180°, and print sends
// exactly the images the preview showed.
func TestPreviewAndPrintFlip(t *testing.T) {
	requirePoppler(t)
	fake, h := newTestServer(t)
	pdf := fixUPS2up(true, 200)
	rec, out := post(t, h, "/api/preview", "ups.pdf", pdf, nil)
	if rec.Code != 200 || out["labels"] != 2.0 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	plain := cropsFrom(t, out, func(int, int) bool { return false })
	first := cropsFrom(t, out, func(_, i int) bool { return i == 0 })
	up, _ := previewLabels(t, h, "ups.pdf", pdf, map[string]string{"crops": plain})
	down, methods := previewLabels(t, h, "ups.pdf", pdf, map[string]string{"crops": first})
	if len(up) != 2 || len(down) != 2 || methods[0] != "native" || methods[1] != "native" {
		t.Fatalf("%d and %d labels, flipped methods %v; want 2 native each", len(up), len(down), methods)
	}
	assertTurned180(t, up[0], down[0])
	assertGray(t, down[1], up[1], "the unflipped box")

	if rec, _ := post(t, h, "/api/print", "ups.pdf", pdf, map[string]string{"crops": first}); rec.Code != 200 {
		t.Fatalf("print: status %d: %s", rec.Code, rec.Body)
	}
	jobs := fake.printJobs()
	if len(jobs) != 2 {
		t.Fatalf("%d jobs, want 2", len(jobs))
	}
	assertGray(t, decodeJob(t, jobs[0]), down[0], "the flipped label as printed")
	assertGray(t, decodeJob(t, jobs[1]), down[1], "the other label as printed")
}

// A flipped image label is still the image's own pixels, turned.
func TestImageFlip(t *testing.T) {
	requirePoppler(t)
	_, h := newTestServer(t)
	data := label200ppi(t) // testdata/label-200ppi.png
	up, um := previewLabels(t, h, "label.png", data, map[string]string{"crops": `{"1": [[0, 0, 1, 1]]}`})
	down, dm := previewLabels(t, h, "label.png", data, map[string]string{"crops": `{"1": [[0, 0, 1, 1, 1]]}`})
	if len(up) != 1 || len(down) != 1 || um[0] != "native" || dm[0] != "native" {
		t.Fatalf("methods %v and %v, want one native label each", um, dm)
	}
	assertTurned180(t, up[0], down[0])
}

// checkResults polls a preview's barcode check until it's done.
func checkResults(t *testing.T, h http.Handler, out map[string]any) []checkLabel {
	t.Helper()
	tok, _ := out["check"].(string)
	if tok == "" {
		t.Fatal("no check token")
	}
	var r checkReply
	for deadline := time.Now().Add(30 * time.Second); !r.Done && time.Now().Before(deadline); {
		var code int
		if code, r = getCheck(t, h, t.Context(), tok); code != 200 {
			t.Fatalf("poll status %d", code)
		}
	}
	if !r.Done {
		t.Fatal("the check didn't finish")
	}
	return r.Labels
}

// A flipped label's barcode check gives what the unflipped one does: 1D
// codes read either way round, and each result keeps its {page, index}.
func TestBarcodeCheckFlipped(t *testing.T) {
	requirePoppler(t)
	_, h := newTestServer(t)
	pdf := fixBarcodes(t)
	fields := map[string]string{"media": "na_index-4x6_4x6in"}
	rec, out := post(t, h, "/api/preview", "labels.pdf", pdf, fields)
	if rec.Code != 200 || out["labels"] != 3.0 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	up := checkResults(t, h, out)
	fields["crops"] = cropsFrom(t, out, func(int, int) bool { return true })
	rec, out = post(t, h, "/api/preview", "labels.pdf", pdf, fields)
	if rec.Code != 200 || out["labels"] != 3.0 {
		t.Fatalf("flipped: status %d: %s", rec.Code, rec.Body)
	}
	down := checkResults(t, h, out)
	if len(up) != 3 || len(down) != 3 {
		t.Fatalf("%d and %d results, want 3 each", len(up), len(down))
	}
	for i := range up {
		u, d := up[i], down[i]
		if d.Page != u.Page || d.Index != u.Index || d.Status != u.Status || d.Carrier != u.Carrier || d.Tracking != u.Tracking {
			t.Errorf("label %d: flipped %+v, unflipped %+v", i, d, u)
		}
	}
	if up[0].Status != checkOK || up[1].Status != checkOK {
		t.Errorf("page 1's labels: %s and %s, want ok", up[0].Status, up[1].Status)
	}
}

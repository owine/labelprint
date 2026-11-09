package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// barcode draws 1D modules in vector at (x, y) inches, module inches per
// module, h inches tall.
func (p *pdfPage) barcode(mods []bool, x, y, module, h float64) {
	for i := 0; i < len(mods); {
		if !mods[i] {
			i++
			continue
		}
		j := i
		for j < len(mods) && mods[j] {
			j++
		}
		p.fillRect(x+float64(i)*module, y, float64(j-i)*module, h)
		i = j
	}
}

// fixBarcodes is synthetic and made-up: page 1 is Letter landscape with two
// 4 × 6 in labels, a UPS 1Z and a USPS IMpb in GS1-128; page 2 is a 4 × 6 in
// label with only a postal routing barcode. Modules are 3 dots at 203 dpi.
func fixBarcodes(t testing.TB) []byte {
	const module = 3.0 / 203
	var b pdfBuilder
	p := b.page(inches(11, 8.5), 0)
	for i, contents := range []string{fakeUPS, gs1IMpbZIP5} {
		x := 0.75 + float64(i)*5.5
		p.strokeRect(x, 1.25, 4, 6, 2)
		p.text(x+0.3, 1.8, 14, "SHIP TO LABELPRINT")
		p.barcode(code128(t, contents), x+0.2, 4.5, module, 1)
	}
	p = b.page(inches(4, 6), 0)
	p.strokeRect(0.05, 0.05, 3.9, 5.9, 2)
	p.barcode(code128(t, gs1Routing), 0.3, 2.5, module, 0.75)
	return b.bytes()
}

// getCheck polls /api/check/{token} once.
func getCheck(t *testing.T, h http.Handler, ctx context.Context, token string) (int, checkReply) {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/check/"+token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r checkReply
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r
}

func TestPreviewBarcodeCheck(t *testing.T) {
	requirePoppler(t)
	_, h := newTestServer(t)
	rec, out := post(t, h, "/api/preview", "labels.pdf", fixBarcodes(t), map[string]string{"media": "na_index-4x6_4x6in"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if out["labels"] != 3.0 {
		t.Fatalf("labels = %v, want 3", out["labels"])
	}
	tok, _ := out["check"].(string)
	if tok == "" {
		t.Fatalf("no check token: %s", rec.Body)
	}
	var r checkReply
	for deadline := time.Now().Add(30 * time.Second); !r.Done && time.Now().Before(deadline); {
		var code int
		if code, r = getCheck(t, h, t.Context(), tok); code != 200 {
			t.Fatalf("poll status %d", code)
		}
	}
	want := []checkLabel{
		{Page: 1, Index: 0, labelCheck: labelCheck{Status: checkOK, Carrier: "UPS", Tracking: fakeUPS}},
		{Page: 1, Index: 1, labelCheck: labelCheck{Status: checkOK, Carrier: "USPS", Tracking: fakeIMpb22}},
		{Page: 2, Index: 0, labelCheck: labelCheck{Status: checkReads}},
	}
	if !r.Done || len(r.Labels) != len(want) {
		t.Fatalf("reply %+v", r)
	}
	for i, w := range want {
		g := r.Labels[i]
		if g.Page != w.Page || g.Index != w.Index || g.Status != w.Status || g.Carrier != w.Carrier || g.Tracking != w.Tracking {
			t.Errorf("label %d: %+v, want %+v", i, g, w)
		}
	}
}

// An error path holds nothing: no token in the reply, nothing in the store.
func TestPreviewErrorHasNoCheck(t *testing.T) {
	requirePoppler(t)
	_, s := newTestServerS(t)
	h := s.routes()
	rec, out := post(t, h, "/api/preview", "labels.pdf", fixBarcodes(t), map[string]string{"pages": "9"})
	if rec.Code < 400 || out["check"] != nil || s.checks.len() != 0 {
		t.Errorf("status %d, check %v, %d tokens held", rec.Code, out["check"], s.checks.len())
	}
}

// A preview whose label PNGs are over the budget on their own gets no token,
// and the page shows no check.
func TestPreviewOverCheckBudget(t *testing.T) {
	requirePoppler(t)
	_, s := newTestServerS(t)
	s.checks.maxBytes = 100
	rec, out := post(t, s.routes(), "/api/preview", "labels.pdf", fixBarcodes(t), nil)
	if rec.Code != 200 || out["check"] != nil || s.checks.len() != 0 {
		t.Errorf("status %d, check %v, %d tokens held", rec.Code, out["check"], s.checks.len())
	}
}

func TestCheckEndpointUnknownToken(t *testing.T) {
	_, h := newTestServer(t)
	if code, _ := getCheck(t, h, t.Context(), "NOPE"); code != http.StatusNotFound {
		t.Errorf("status %d, want 404", code)
	}
}

// A client that goes away ends its long poll at once, not after 8 s.
func TestCheckEndpointClientGone(t *testing.T) {
	_, s := newTestServerS(t)
	dec, _, _ := blockingDecode(t)
	s.checks.decode = dec
	tok, _ := s.checks.put(s.baseCtx, pngs(t, 1))
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	getCheck(t, s.routes(), ctx, tok)
	if took := time.Since(start); took > time.Second {
		t.Errorf("the handler ran %s after the client left", took)
	}
}

// ZPL gets its tracking from the source, and no check token.
func TestPreviewZPLTracking(t *testing.T) {
	_, h := newTestServer(t)
	zpl := "^XA^FO50,50^BY3^BCN,200,Y,N,N^FD>:1ZA1B2C3>54567890123^FS^XZ"
	rec, out := post(t, h, "/api/preview", "label.zpl", []byte(zpl), nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	tr, _ := out["tracking"].([]any)
	if len(tr) != 1 || out["check"] != nil {
		t.Fatalf("tracking %v, check %v", out["tracking"], out["check"])
	}
	if e := tr[0].(map[string]any); e["carrier"] != "UPS" || e["value"] != fakeUPSDigits {
		t.Errorf("tracking %v", e)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is an injectable clock for the share store.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestStore(maxItems int, maxBytes int64) (*shareStore, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	st := &shareStore{ttl: 10 * time.Minute, maxItems: maxItems, maxBytes: maxBytes, now: clock.now}
	return st, clock
}

func mustPut(t *testing.T, st *shareStore, name string, data []byte) string {
	t.Helper()
	tok, err := st.put(name, data, kindZPL)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// ---------------------------------------------------------------- store

func TestShareStoreSingleUse(t *testing.T) {
	st, _ := newTestStore(8, 1<<20)
	tok := mustPut(t, st, "a.zpl", []byte("^XA^XZ"))
	if len(tok) < 22 || strings.ContainsAny(tok, "/+=?&#%") {
		t.Fatalf("token %q isn't a long url-safe string", tok)
	}
	f, ok := st.take(tok)
	if !ok || f.name != "a.zpl" || string(f.data) != "^XA^XZ" || f.kind != kindZPL {
		t.Fatalf("take = %+v, %v", f, ok)
	}
	if _, ok := st.take(tok); ok {
		t.Fatal("a token worked twice")
	}
	if st.len() != 0 || st.size() != 0 {
		t.Fatalf("store still holds %d items / %d bytes", st.len(), st.size())
	}
}

func TestShareStoreTokensAreUnique(t *testing.T) {
	st, _ := newTestStore(1000, 1<<20)
	seen := map[string]bool{}
	for range 500 {
		tok := mustPut(t, st, "a.zpl", []byte("x"))
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

func TestShareStoreExpiry(t *testing.T) {
	st, clock := newTestStore(8, 1<<20)
	old := mustPut(t, st, "old.zpl", []byte("old"))
	clock.advance(9 * time.Minute)
	fresh := mustPut(t, st, "new.zpl", []byte("new"))
	clock.advance(time.Minute + time.Second)

	if _, ok := st.take(old); ok {
		t.Fatal("an expired share was handed out")
	}
	// sweep drops expired items without anyone asking for them
	st.sweep()
	if st.len() != 1 || st.size() != 3 {
		t.Fatalf("after sweep: %d items / %d bytes, want 1 / 3", st.len(), st.size())
	}
	if f, ok := st.take(fresh); !ok || string(f.data) != "new" {
		t.Fatal("the unexpired share is gone")
	}
}

func TestShareStoreSweepOnPut(t *testing.T) {
	st, clock := newTestStore(8, 1<<20)
	mustPut(t, st, "a.zpl", []byte("aaaa"))
	clock.advance(11 * time.Minute)
	mustPut(t, st, "b.zpl", []byte("b"))
	if st.len() != 1 || st.size() != 1 {
		t.Fatalf("expired item survived a put: %d items / %d bytes", st.len(), st.size())
	}
}

func TestShareStoreItemCapEvictsOldest(t *testing.T) {
	st, clock := newTestStore(3, 1<<20)
	var toks []string
	for i := range 4 {
		toks = append(toks, mustPut(t, st, fmt.Sprintf("%d.zpl", i), []byte{byte('0' + i)}))
		clock.advance(time.Second)
	}
	if st.len() != 3 {
		t.Fatalf("%d items held, cap is 3", st.len())
	}
	if _, ok := st.take(toks[0]); ok {
		t.Fatal("the oldest share should have been evicted")
	}
	for _, tok := range toks[1:] {
		if _, ok := st.take(tok); !ok {
			t.Fatal("a newer share was evicted")
		}
	}
}

func TestShareStoreByteCapEvictsOldest(t *testing.T) {
	st, clock := newTestStore(8, 10)
	a := mustPut(t, st, "a.zpl", bytes.Repeat([]byte("a"), 4))
	clock.advance(time.Second)
	b := mustPut(t, st, "b.zpl", bytes.Repeat([]byte("b"), 4))
	clock.advance(time.Second)
	c := mustPut(t, st, "c.zpl", bytes.Repeat([]byte("c"), 4)) // 12 > 10: a goes
	if st.size() != 8 {
		t.Fatalf("holding %d bytes, want 8", st.size())
	}
	if _, ok := st.take(a); ok {
		t.Fatal("oldest share survived the byte cap")
	}
	for _, tok := range []string{b, c} {
		if _, ok := st.take(tok); !ok {
			t.Fatal("a newer share was evicted")
		}
	}
	if _, err := st.put("huge.zpl", bytes.Repeat([]byte("h"), 11), kindZPL); err == nil {
		t.Fatal("a share larger than the whole store was accepted")
	}
}

func TestShareStoreConcurrent(t *testing.T) {
	st, clock := newTestStore(8, 1<<20)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 200 {
				tok, err := st.put("x.zpl", []byte(fmt.Sprint(g, i)), kindZPL)
				if err != nil {
					t.Error(err)
					return
				}
				st.take(tok)
				if i%50 == 0 {
					clock.advance(time.Minute)
					st.sweep()
				}
			}
		})
	}
	wg.Wait()
	if st.len() > 8 || st.size() < 0 {
		t.Fatalf("%d items / %d bytes after concurrent use", st.len(), st.size())
	}
	// whatever is left must account for itself exactly
	var total int64
	for _, f := range st.items {
		total += int64(len(f.data))
	}
	if total != st.size() {
		t.Fatalf("byte count %d, items hold %d", st.size(), total)
	}
}

// ---------------------------------------------------------------- handlers

func sharePost(t *testing.T, h http.Handler, filename string, data []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, filename, data, nil)
	req := httptest.NewRequest(http.MethodPost, "/share", body)
	req.Header.Set("Content-Type", ct)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func shareGet(h http.Handler, token string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/share/"+token, nil))
	return rec
}

// tokenFrom pulls the token out of a /?share=<token> location.
func tokenFrom(t *testing.T, loc string) string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil || u.Path != "/" {
		t.Fatalf("location %q isn't /?share=…", loc)
	}
	tok := u.Query().Get("share")
	if tok == "" {
		t.Fatalf("location %q has no token", loc)
	}
	return tok
}

func TestShareRedirectsToPreview(t *testing.T) {
	fake, h := newTestServer(t)
	zpl := []byte("^XA^FDhello^FS^XZ")
	// what Android Chrome sends: a same-origin POST navigation
	rec := sharePost(t, h, "return label.zpl", zpl, map[string]string{
		"Sec-Fetch-Site": "same-origin", "Accept": "text/html,application/xhtml+xml,*/*;q=0.8"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q", cc)
	}
	tok := tokenFrom(t, rec.Header().Get("Location"))

	got := shareGet(h, tok)
	if got.Code != 200 || !bytes.Equal(got.Body.Bytes(), zpl) {
		t.Fatalf("status %d: %q", got.Code, got.Body)
	}
	if cc := got.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q", cc)
	}
	_, params, err := mime.ParseMediaType(got.Header().Get("Content-Disposition"))
	if err != nil || params["filename"] != "return label.zpl" {
		t.Errorf("content-disposition = %q (%v)", got.Header().Get("Content-Disposition"), err)
	}
	if ct := got.Header().Get("Content-Type"); ct != "application/vnd.zebra-zpl" {
		t.Errorf("content-type = %q", ct)
	}

	again := shareGet(h, tok)
	if again.Code != 404 || !strings.Contains(again.Body.String(), "expired") {
		t.Fatalf("second fetch: status %d: %s", again.Code, again.Body)
	}
	if len(fake.printJobs()) != 0 {
		t.Fatal("sharing printed")
	}
}

func TestShareFilenameRoundTripsUnicode(t *testing.T) {
	_, h := newTestServer(t)
	rec := sharePost(t, h, "étiquette retour.pdf", []byte("%PDF-1.4\n"), nil)
	got := shareGet(h, tokenFrom(t, rec.Header().Get("Location")))
	_, params, err := mime.ParseMediaType(got.Header().Get("Content-Disposition"))
	if err != nil || params["filename"] != "étiquette retour.pdf" {
		t.Fatalf("content-disposition = %q (%v)", got.Header().Get("Content-Disposition"), err)
	}
	if ct := got.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("content-type = %q", ct)
	}
}

func TestShareJSON(t *testing.T) {
	_, h := newTestServer(t)
	png := []byte("\x89PNG\r\n\x1a\nrest")
	// what an iOS Shortcut sends: no Sec-Fetch-Site, no Origin
	rec := sharePost(t, h, "label.png", png, map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	got := shareGet(h, tokenFrom(t, out.URL))
	if got.Code != 200 || !bytes.Equal(got.Body.Bytes(), png) {
		t.Fatalf("status %d", got.Code)
	}
}

func TestShareRejects(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		data     []byte
		maxBytes int64
		status   int    // JSON
		code     string // redirect's share-error
	}{
		{"bmp", "x.bmp", []byte("BM......"), 0, 415, "unsupported"},
		{"plain text", "notes.txt", []byte("hello"), 0, 415, "unsupported"},
		{"too big", "big.zpl", bytes.Repeat([]byte("^XA^XZ"), 400), 1024, 413, "too-big"},
		{"empty", "a.zpl", nil, 0, 400, "unreadable"},
		{"epl2", "a.zpl", []byte(epl2Sample), 0, 415, "epl2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, s := newTestServerS(t, func(cfg *config) {
				if c.maxBytes > 0 {
					cfg.MaxUpload = c.maxBytes
				}
			})
			h := s.routes()

			rec := sharePost(t, h, c.file, c.data, map[string]string{"Accept": "application/json"})
			var out map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if rec.Code != c.status || out["error"] == "" {
				t.Fatalf("json: status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}

			rec = sharePost(t, h, c.file, c.data, map[string]string{"Sec-Fetch-Site": "same-origin"})
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("navigation: status %d: %s", rec.Code, rec.Body)
			}
			if loc := rec.Header().Get("Location"); loc != "/?share-error="+c.code {
				t.Fatalf("location = %q, want /?share-error=%s", loc, c.code)
			}
			if n := s.shares.len(); n != 0 {
				t.Fatalf("a rejected upload was stored (%d held)", n)
			}
		})
	}
}

func TestShareNotMultipart(t *testing.T) {
	_, h := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/share", strings.NewReader("file=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestShareCrossSiteRejected(t *testing.T) {
	_, s := newTestServerS(t)
	h := s.routes()
	for _, hdr := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Origin": "https://evil.example"}, // an old browser without Sec-Fetch-Site
	} {
		rec := sharePost(t, h, "a.zpl", []byte("^XA^XZ"), hdr)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%v: status %d", hdr, rec.Code)
		}
	}
	if n := s.shares.len(); n != 0 {
		t.Fatalf("a cross-site share was stored (%d held)", n)
	}
	// What a share target launched from the OS share sheet may send instead
	// of same-origin: allowed.
	if rec := sharePost(t, h, "a.zpl", []byte("^XA^XZ"), map[string]string{"Sec-Fetch-Site": "none"}); rec.Code != http.StatusSeeOther {
		t.Errorf("Sec-Fetch-Site none: status %d", rec.Code)
	}
}

func TestShareGetUnknown(t *testing.T) {
	_, h := newTestServer(t)
	for _, tok := range []string{"nope", "AAAAAAAAAAAAAAAAAAAAAAAAAA", strings.Repeat("x", 300)} {
		rec := shareGet(h, tok)
		if rec.Code != 404 {
			t.Errorf("%q: status %d", tok, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%q: content-type %q", tok, ct)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/share", nil))
	if rec.Code == 200 {
		t.Error("GET /share should not serve anything")
	}
}

func TestManifestAndWorker(t *testing.T) {
	_, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var m struct {
		StartURL    string `json:"start_url"`
		Scope       string `json:"scope"`
		Display     string `json:"display"`
		Icons       []struct{ Src, Sizes, Type string }
		ShareTarget struct {
			Action, Method, Enctype string
			Params                  struct {
				Files []struct {
					Name   string
					Accept []string
				}
			}
		} `json:"share_target"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	st := m.ShareTarget
	if st.Action != "/share" || st.Method != "POST" || st.Enctype != "multipart/form-data" ||
		len(st.Params.Files) != 1 || st.Params.Files[0].Name != "file" {
		t.Fatalf("share_target = %+v", st)
	}
	if accept := strings.Join(st.Params.Files[0].Accept, " "); !strings.Contains(accept, "image/gif") ||
		!strings.Contains(accept, ".gif") {
		t.Errorf("share_target doesn't accept gifs: %s", accept)
	}
	if m.StartURL != "/" || m.Scope != "/" || m.Display != "standalone" {
		t.Errorf("start_url %q scope %q display %q", m.StartURL, m.Scope, m.Display)
	}
	// Chrome's install criteria: a 192px and a 512px icon, and they must exist
	sizes := map[string]bool{}
	for _, ic := range m.Icons {
		sizes[ic.Sizes] = true
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, ic.Src, nil))
		if r.Code != 200 || r.Header().Get("Content-Type") != ic.Type {
			t.Errorf("icon %s: status %d, type %q", ic.Src, r.Code, r.Header().Get("Content-Type"))
		}
	}
	if !sizes["192x192"] || !sizes["512x512"] {
		t.Errorf("icons = %+v", m.Icons)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sw.js", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("sw.js: status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// the worker must never see a request, let alone cache one
	body, _ := io.ReadAll(rec.Body)
	for _, banned := range []string{"addEventListener('fetch'", `addEventListener("fetch"`, "onfetch", "caches."} {
		if bytes.Contains(body, []byte(banned)) {
			t.Errorf("sw.js contains %q", banned)
		}
	}
}

// Held shares stay within a fixed memory budget whatever MAX_UPLOAD_MB is, but
// always fit at least one upload.
func TestShareStoreByteCap(t *testing.T) {
	for _, c := range []struct{ upload, want int64 }{
		{20 << 20, 80 << 20},   // the default: four full-size shares
		{5 << 20, 80 << 20},    // small uploads: the item cap binds first
		{200 << 20, 200 << 20}, // the largest MAX_UPLOAD_MB: one share, not 800 MB
	} {
		if got := newShareStore(c.upload).maxBytes; got != c.want {
			t.Errorf("upload cap %d MB: store holds %d MB, want %d MB", c.upload>>20, got>>20, c.want>>20)
		}
	}
}

// The iOS Shortcut downloads as a file, so iPhone Safari offers to add it, and
// it's the signed build: an unsigned plist won't import on iOS.
func TestShortcutDownload(t *testing.T) {
	_, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/labelprint.shortcut", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "attachment; filename=labelprint.shortcut" {
		t.Errorf("Content-Disposition %q", cd)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("security headers missing")
	}
	if body := rec.Body.Bytes(); !bytes.HasPrefix(body, []byte("AEA1")) {
		t.Errorf("not a signed shortcut (AEA1 archive): starts %q", body[:min(8, len(body))])
	}
}

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Sharing from a phone: the Android share sheet (the manifest's share_target)
// and the iOS Shortcut both POST the file to /share. It's checked like an
// upload to /api/print, held here in memory under a one-time token, and the
// phone is sent to /?share=<token>, where the page fetches it from
// /api/share/{token} and shows the normal preview. A share never prints; the
// user still presses print.
//
// Labels carry names and addresses, so a held file never touches disk, is
// never logged and is dropped after one fetch or ten minutes, whichever comes
// first.

func init() {
	// Go's built-in table lacks it, and the image has no /etc/mime.types.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

const (
	shareTTL      = 10 * time.Minute
	shareMaxItems = 8
)

type sharedFile struct {
	name  string
	data  []byte
	kind  docKind
	added time.Time
}

// shareStore holds shared files until the page collects them. Both caps evict
// the oldest share rather than refuse a new one: the newest share is the one
// someone is waiting on.
type shareStore struct {
	mu       sync.Mutex
	items    map[string]*sharedFile
	bytes    int64
	ttl      time.Duration
	maxItems int
	maxBytes int64
	now      func() time.Time
}

// shareBudget is the memory held shares may use: four full-size uploads at
// the default 20 MB cap. It's fixed rather than a multiple of MAX_UPLOAD_MB,
// which goes up to 200 MB (4× that would outgrow a small container), but
// never less than one upload, so any file that can be uploaded can be shared.
const shareBudget = 80 << 20

func newShareStore(maxUpload int64) *shareStore {
	return &shareStore{ttl: shareTTL, maxItems: shareMaxItems, maxBytes: max(maxUpload, shareBudget), now: time.Now}
}

// Can't happen while maxBytes is at least the upload cap, but put shouldn't
// evict everything for a file that still won't fit.
var errShareTooBig = &userError{Status: http.StatusRequestEntityTooLarge, Msg: "that file is too big to share"}

// put holds data (not a copy) and returns its token: 128 random bits, base32
// (A–Z, 2–7), so it's URL-safe as is.
func (st *shareStore) put(name string, data []byte, kind docKind) (string, error) {
	if int64(len(data)) > st.maxBytes {
		return "", errShareTooBig
	}
	token := rand.Text()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.items == nil {
		st.items = map[string]*sharedFile{}
	}
	now := st.now()
	st.sweepLocked(now)
	for len(st.items) >= st.maxItems || st.bytes+int64(len(data)) > st.maxBytes {
		st.dropLocked(st.oldestLocked())
	}
	st.items[token] = &sharedFile{name: name, data: data, kind: kind, added: now}
	st.bytes += int64(len(data))
	return token, nil
}

// take hands a share out once and forgets it.
func (st *shareStore) take(token string) (*sharedFile, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	f, ok := st.items[token]
	if !ok {
		return nil, false
	}
	delete(st.items, token)
	st.bytes -= int64(len(f.data))
	if st.expired(f, st.now()) {
		clear(f.data)
		return nil, false
	}
	return f, true
}

// sweep drops expired shares. put sweeps too; the janitor covers the case
// where nobody shares or collects anything for a while.
func (st *shareStore) sweep() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked(st.now())
}

func (st *shareStore) janitor(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st.sweep()
		}
	}
}

func (st *shareStore) expired(f *sharedFile, now time.Time) bool {
	return now.Sub(f.added) >= st.ttl
}

func (st *shareStore) sweepLocked(now time.Time) {
	for tok, f := range st.items {
		if st.expired(f, now) {
			st.dropLocked(tok)
		}
	}
}

func (st *shareStore) oldestLocked() string {
	var oldest string
	var at time.Time
	for tok, f := range st.items {
		if oldest == "" || f.added.Before(at) {
			oldest, at = tok, f.added
		}
	}
	return oldest
}

// dropLocked forgets a share and zeroes its bytes, so the label doesn't
// linger in freed memory until the GC reuses it.
func (st *shareStore) dropLocked(token string) {
	if f, ok := st.items[token]; ok {
		delete(st.items, token)
		st.bytes -= int64(len(f.data))
		clear(f.data)
	}
}

func (st *shareStore) len() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.items)
}

func (st *shareStore) size() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.bytes
}

// ---------------------------------------------------------------- handlers

// handleShare stages an upload for the preview. A browser (the share sheet's
// POST navigation) is redirected to the page; a client that asks for JSON
// (the iOS Shortcut) gets {"url": "/?share=<token>"} to open itself.
//
// It sits behind CrossOriginProtection like every POST: Android Chrome sends
// the share as a same-origin navigation, and a Shortcut sends no browser
// headers, so both pass.
func (s *server) handleShare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	wantsJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	up, err := s.readUpload(w, r)
	if err == nil {
		var token string
		if token, err = s.shares.put(up.Filename, up.Data, up.Kind); err == nil {
			s.log.Info("share staged", "kind", up.Kind, "bytes", len(up.Data))
			loc := "/?share=" + token
			if wantsJSON {
				writeJSON(w, http.StatusOK, map[string]string{"url": loc})
				return
			}
			http.Redirect(w, r, loc, http.StatusSeeOther)
			return
		}
	}
	s.log.Info("share rejected", "reason", shareErrorCode(err))
	if wantsJSON {
		s.fail(w, err)
		return
	}
	// The share sheet opened this as a page: land on the page either way and
	// let it explain. A fixed code, not the message, so a link can't put
	// arbitrary text in front of the user.
	http.Redirect(w, r, "/?share-error="+shareErrorCode(err), http.StatusSeeOther)
}

func shareErrorCode(err error) string {
	var ue *userError
	if errors.As(err, &ue) {
		if ue == errEPL2 {
			return "epl2"
		}
		switch ue.Status {
		case http.StatusRequestEntityTooLarge:
			return "too-big"
		case http.StatusUnsupportedMediaType:
			return "unsupported"
		}
	}
	return "unreadable"
}

// handleSharedFile hands a staged file to the page, once.
func (s *server) handleSharedFile(w http.ResponseWriter, r *http.Request) {
	f, ok := s.shares.take(r.PathValue("token"))
	if !ok {
		s.log.Info("share fetch", "outcome", "missing")
		http.Error(w, "that shared file has expired — share it again", http.StatusNotFound)
		return
	}
	defer clear(f.data)
	ct := f.kind.mimeType()
	if f.kind == kindPDF {
		ct = "application/pdf"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", contentDisposition(f.name))
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(f.data)
	s.log.Info("share fetch", "outcome", "served", "kind", f.kind, "bytes", len(f.data))
}

// contentDisposition carries the filename: plain ASCII names as filename=,
// anything else RFC 2231-encoded as filename*= (mime.FormatMediaType picks).
func contentDisposition(name string) string {
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return "attachment; filename*=utf-8''" + url.PathEscape(name)
}

// shortcutHandler serves the signed iOS Shortcut (web/labelprint.shortcut,
// built from shortcut/labelprint.shortcut.xml) as a download. iPhone Safari
// saves it, and opening it offers to add the shortcut, which asks for this
// labelprint's address.
func shortcutHandler(static fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment; filename=labelprint.shortcut")
		http.ServeFileFS(w, r, static, "labelprint.shortcut")
	})
}

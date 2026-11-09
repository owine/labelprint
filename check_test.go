package main

import (
	"bytes"
	"context"
	"image"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// syncBuffer is a log sink the store's goroutines can write to while a test
// reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testCheckStore is a store with a captured log and short timings.
func testCheckStore(t *testing.T) (*checkStore, *syncBuffer) {
	t.Helper()
	var logs syncBuffer
	// no time: in UTC it ends in "<digit>Z", which the leak checks would
	// mistake for a UPS number's "1Z"
	noTime := func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}
	st := newCheckStore(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: noTime})))
	st.unpolled, st.timeout, st.pollWait, st.heldAfter = 100*time.Millisecond, time.Second, time.Second, time.Second
	return st, &logs
}

// pngs is n blank label PNGs.
func pngs(t *testing.T, n int) []checkInput {
	t.Helper()
	data := encodePNG(t, newWhite(64, 64))
	out := make([]checkInput, n)
	for i := range out {
		out[i] = checkInput{Page: 1, Index: i, PNG: data}
	}
	return out
}

// blockingDecode never returns until release is closed, and counts its calls.
func blockingDecode(t *testing.T) (decode func(*image.Gray) labelCheck, calls *atomic.Int32, release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	calls = new(atomic.Int32)
	return func(*image.Gray) labelCheck {
		calls.Add(1)
		<-ch
		return labelCheck{Status: checkNone}
	}, calls, release
}

func mustCheck(t *testing.T, st *checkStore, labels []checkInput) string {
	t.Helper()
	tok, ok := st.put(t.Context(), labels)
	if !ok {
		t.Fatal("put refused")
	}
	return tok
}

// eventually polls cond for up to 2 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCheckPollDone(t *testing.T) {
	st, _ := testCheckStore(t)
	st.decode = func(*image.Gray) labelCheck { return labelCheck{Status: checkOK, Carrier: "UPS", Tracking: fakeUPS} }
	labels := pngs(t, 2)
	labels[1].Page, labels[1].Index = 3, 0
	tok := mustCheck(t, st, labels)
	r, ok := st.poll(t.Context(), tok)
	if !ok || !r.Done || len(r.Labels) != 2 {
		t.Fatalf("poll: %v %+v", ok, r)
	}
	if l := r.Labels[1]; l.Page != 3 || l.Index != 0 || l.Status != checkOK || l.Tracking != fakeUPS {
		t.Errorf("label 2: %+v", l)
	}
	if st.size() != 0 {
		t.Errorf("%d PNG bytes still held after decoding", st.size())
	}
	// results stay readable, unlike a share
	if r, ok := st.poll(t.Context(), tok); !ok || !r.Done {
		t.Errorf("second poll: %v %+v", ok, r)
	}
}

func TestCheckPollWaitsThenAnswersPending(t *testing.T) {
	st, _ := testCheckStore(t)
	st.pollWait = 100 * time.Millisecond
	st.decode, _, _ = blockingDecode(t)
	tok := mustCheck(t, st, pngs(t, 1))
	start := time.Now()
	r, ok := st.poll(t.Context(), tok)
	if !ok || r.Done || r.Labels[0].Status != checkPending {
		t.Fatalf("poll: %v %+v", ok, r)
	}
	if took := time.Since(start); took < 100*time.Millisecond || took > time.Second {
		t.Errorf("answered after %s, want the 100 ms wait", took)
	}
}

func TestCheckUnknownToken(t *testing.T) {
	st, _ := testCheckStore(t)
	if _, ok := st.poll(t.Context(), "NOPE"); ok {
		t.Error("an unknown token was found")
	}
}

// Nothing is decoded until the first poll: the page only polls a preview it
// shows.
func TestCheckStartsOnFirstPoll(t *testing.T) {
	st, _ := testCheckStore(t)
	var calls atomic.Int32
	st.decode = func(*image.Gray) labelCheck { calls.Add(1); return labelCheck{Status: checkNone} }
	tok := mustCheck(t, st, pngs(t, 1))
	time.Sleep(200 * time.Millisecond) // past unpolled: a never-polled token isn't cancelled
	if calls.Load() != 0 {
		t.Fatal("decoded before anyone polled")
	}
	if r, ok := st.poll(t.Context(), tok); !ok || !r.Done || calls.Load() != 1 {
		t.Errorf("poll: %v %+v, %d decodes", ok, r, calls.Load())
	}
}

func TestCheckUnpolledIsCancelled(t *testing.T) {
	st, _ := testCheckStore(t)
	decode, calls, _ := blockingDecode(t)
	st.decode = decode
	tok := mustCheck(t, st, pngs(t, 2))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, ok := st.poll(ctx, tok); !ok { // the page aborts its poll
		t.Fatal("poll refused")
	}
	st.mu.Lock()
	it := st.items[tok]
	st.mu.Unlock()
	eventually(t, "the unpolled token to be cancelled", func() bool { return st.len() == 0 })
	if it.ctx.Err() == nil {
		t.Error("its context wasn't cancelled")
	}
	if _, ok := st.poll(t.Context(), tok); ok {
		t.Error("a cancelled token was found")
	}
	if st.size() != 0 {
		t.Errorf("%d PNG bytes still held", st.size())
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d decodes, want only the one already running", n)
	}
}

// A long poll in progress always counts as polling, however long it waits.
func TestCheckOpenPollIsNeverCancelled(t *testing.T) {
	st, _ := testCheckStore(t)
	st.pollWait = 500 * time.Millisecond // five times unpolled
	st.decode, _, _ = blockingDecode(t)
	tok := mustCheck(t, st, pngs(t, 1))
	if r, ok := st.poll(t.Context(), tok); !ok || r.Done {
		t.Fatalf("poll: %v %+v", ok, r)
	}
	if st.len() != 1 {
		t.Fatal("cancelled during an open poll")
	}
	// and polled again well within unpolled, it stays
	time.Sleep(20 * time.Millisecond)
	if _, ok := st.poll(t.Context(), tok); !ok {
		t.Fatal("cancelled although re-polled in time")
	}
}

// A finished token isn't cancelled for going unpolled: its results stay
// readable for the TTL.
func TestCheckDoneTokenOutlivesUnpolled(t *testing.T) {
	st, _ := testCheckStore(t)
	st.decode = func(*image.Gray) labelCheck { return labelCheck{Status: checkNone} }
	tok := mustCheck(t, st, pngs(t, 1))
	if r, ok := st.poll(t.Context(), tok); !ok || !r.Done {
		t.Fatalf("poll: %v %+v", ok, r)
	}
	time.Sleep(3 * st.unpolled)
	if _, ok := st.poll(t.Context(), tok); !ok {
		t.Error("a finished token was dropped for going unpolled")
	}
}

func TestCheckTTL(t *testing.T) {
	st, _ := testCheckStore(t)
	now := time.Now()
	st.now = func() time.Time { return now }
	tok := mustCheck(t, st, pngs(t, 1))
	now = now.Add(checkTTL)
	if _, ok := st.poll(t.Context(), tok); ok {
		t.Error("an expired token was found")
	}
	other := mustCheck(t, st, pngs(t, 1))
	now = now.Add(checkTTL)
	st.sweep()
	if st.len() != 0 || st.size() != 0 {
		t.Errorf("sweep left %d tokens, %d bytes (%s)", st.len(), st.size(), other)
	}
}

func TestCheckCapDropsOldest(t *testing.T) {
	st, _ := testCheckStore(t)
	now := time.Now()
	st.now = func() time.Time { now = now.Add(time.Second); return now }
	st.maxItems = 2
	first := mustCheck(t, st, pngs(t, 1))
	st.mu.Lock()
	it := st.items[first]
	st.mu.Unlock()
	mustCheck(t, st, pngs(t, 1))
	mustCheck(t, st, pngs(t, 1))
	if st.len() != 2 {
		t.Fatalf("%d tokens, want 2", st.len())
	}
	if _, ok := st.poll(t.Context(), first); ok {
		t.Error("the oldest token survived the cap")
	}
	if it.ctx.Err() == nil {
		t.Error("the dropped token's work wasn't cancelled")
	}
}

func TestCheckBudget(t *testing.T) {
	st, _ := testCheckStore(t)
	now := time.Now()
	st.now = func() time.Time { now = now.Add(time.Second); return now }
	one := pngs(t, 1)
	size := int64(len(one[0].PNG))
	st.maxBytes = 3 * size
	// over the budget on its own: no token, nothing held
	if _, ok := st.put(t.Context(), pngs(t, 4)); ok || st.len() != 0 {
		t.Fatal("an over-budget preview got a token")
	}
	if _, ok := st.put(t.Context(), nil); ok {
		t.Fatal("nothing to check got a token")
	}
	a := mustCheck(t, st, pngs(t, 2))
	mustCheck(t, st, pngs(t, 1))
	// fits alone, not beside the two pending: the oldest goes
	mustCheck(t, st, pngs(t, 2))
	if _, ok := st.poll(t.Context(), a); ok {
		t.Error("the oldest token wasn't evicted for the budget")
	}
	if st.size() != 3*size {
		t.Errorf("%d bytes held, want %d", st.size(), 3*size)
	}
}

// A decode past its timeout is reported and abandoned, but keeps the one
// slot until it really returns; a second token waits for the slot, and
// cancelling it while it waits means it never decodes.
func TestCheckTimeoutHoldsSlot(t *testing.T) {
	st, logs := testCheckStore(t)
	st.timeout, st.heldAfter = 50*time.Millisecond, 100*time.Millisecond
	decode, calls, release := blockingDecode(t)
	st.decode = decode
	a := mustCheck(t, st, pngs(t, 1))
	r, ok := st.poll(t.Context(), a)
	if !ok || !r.Done || r.Labels[0].Status != checkTimedOut {
		t.Fatalf("poll a: %v %+v", ok, r)
	}
	if len(st.slot) != 1 {
		t.Fatal("the timed-out decode gave up the slot")
	}
	st.pollWait = 100 * time.Millisecond
	b := mustCheck(t, st, pngs(t, 1))
	if r, ok := st.poll(t.Context(), b); !ok || r.Done || r.Labels[0].Status != checkPending {
		t.Fatalf("poll b: %v %+v", ok, r)
	}
	eventually(t, "the held-slot log", func() bool { return strings.Contains(logs.String(), "check slot held past timeout") })
	st.mu.Lock()
	st.dropLocked(b)
	st.mu.Unlock()
	release()
	eventually(t, "the slot to free", func() bool { return len(st.slot) == 0 })
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("%d decodes, want 1: the cancelled token must never start", n)
	}
	if n := strings.Count(logs.String(), "check slot held past timeout"); n != 1 {
		t.Errorf("held-slot logged %d times, want once", n)
	}
}

func TestCheckPanicLogsNoValue(t *testing.T) {
	st, logs := testCheckStore(t)
	st.decode = func(*image.Gray) labelCheck { panic("decoded " + fakeUPS) }
	tok := mustCheck(t, st, pngs(t, 1))
	r, ok := st.poll(t.Context(), tok)
	if !ok || !r.Done || r.Labels[0].Status != checkError {
		t.Fatalf("poll: %v %+v", ok, r)
	}
	out := logs.String()
	if !strings.Contains(out, "check decode panicked") || !strings.Contains(out, "error=1") {
		t.Errorf("log: %s", out)
	}
	if strings.Contains(out, fakeUPS) || strings.Contains(out, "decoded") {
		t.Errorf("the panic value reached the log: %s", out)
	}
}

// The log carries counts only: never a tracking value, nor gozxing's error
// text.
func TestCheckLogHasNoValues(t *testing.T) {
	st, logs := testCheckStore(t)
	g := label4x6()
	drawBars(g, code128(t, fakeUPS), 40, 700, 3, 200, false)
	tok := mustCheck(t, st, []checkInput{{Page: 1, Index: 0, PNG: encodePNG(t, g)}, {Page: 1, Index: 1, PNG: encodePNG(t, label4x6())}})
	r, ok := st.poll(t.Context(), tok)
	if !ok || !r.Done || r.Labels[0].Tracking != fakeUPS || r.Labels[1].Status != checkNone {
		t.Fatalf("poll: %v %+v", ok, r)
	}
	out := logs.String()
	want := "msg=check token=" + tok[:6] + " labels=2 ok=1 reads=0 none=1 timeout=0 error=0 ms="
	if !strings.Contains(out, want) {
		t.Errorf("log %q lacks %q", out, want)
	}
	// (not "UPS": the token's 6-character prefix in the log could spell it)
	for _, leak := range []string{fakeUPS, "1Z", "carrier", "code128", "NotFound", "Exception", tok} {
		if strings.Contains(out, leak) {
			t.Errorf("log has %q: %s", leak, out)
		}
	}
}

// Shutdown (the base context ending) cancels waiting work.
func TestCheckShutdownCancelsWaiting(t *testing.T) {
	st, _ := testCheckStore(t)
	decode, calls, release := blockingDecode(t)
	st.decode = decode
	st.pollWait = 100 * time.Millisecond
	base, shutdown := context.WithCancel(t.Context())
	tok, _ := st.put(base, pngs(t, 2))
	st.poll(t.Context(), tok)
	shutdown()
	release()
	eventually(t, "the slot to free", func() bool { return len(st.slot) == 0 })
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("%d decodes after shutdown, want only the one running", n)
	}
}

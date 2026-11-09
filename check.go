package main

import (
	"context"
	"crypto/rand"
	"image"
	"log/slog"
	"sync"
	"time"
)

// The check store holds a preview's label PNGs under a token until the page
// asks for their barcode check, then decodes them in the background, one at
// a time, and keeps the results for the page to collect.
//
// Nothing is decoded until the first GET /api/check/{token}: the page polls
// only a preview it shows. Once polled, a token nobody is polling any more
// (no request open, and the last one ended over checkUnpolled ago) is
// cancelled, so an out-of-date preview's work stops. A token that was never
// polled (a reply held during a drag) waits, holding only its PNGs, until
// the cap, the budget or the TTL drops it.
//
// Labels carry names and addresses: PNGs and results stay in memory, are
// sent only in reply to the token, and are never logged.

const (
	checkTTL      = 10 * time.Minute
	checkMaxItems = 32
	checkBudget   = 32 << 20 // pending PNG bytes across tokens
)

// checkInput is one label to check: page and index say which (index into
// that page's labelImages).
type checkInput struct {
	Page, Index int
	PNG         []byte
}

// checkLabel is one label's entry in a poll reply.
type checkLabel struct {
	Page  int `json:"page"`
	Index int `json:"index"`
	labelCheck
}

// checkReply is what GET /api/check/{token} answers.
type checkReply struct {
	Done   bool         `json:"done"`
	Labels []checkLabel `json:"labels"`
}

type checkItem struct {
	token   string
	added   time.Time
	ctx     context.Context
	cancel  context.CancelFunc
	pngs    [][]byte // parallel to labels; nil once decoded
	labels  []checkLabel
	bytes   int64 // pending PNG bytes
	started bool
	done    chan struct{} // closed once every label has a result
	open    int           // poll requests open now
	lastEnd time.Time     // when the last one ended
	idle    *time.Timer   // cancels the token if it stays unpolled
}

type checkStore struct {
	mu       sync.Mutex
	items    map[string]*checkItem
	bytes    int64
	now      func() time.Time
	log      *slog.Logger
	slot     chan struct{} // one decode at a time: PDF and ZPL hold two heavy slots each already
	heldOnce sync.Once     // the held-slot warning, once a process: a hung decode never returns

	// decode is decodeLabel; tests swap in panicking and slow ones.
	decode func(*image.Gray) labelCheck

	ttl, unpolled, timeout, pollWait, heldAfter time.Duration
	maxItems                                    int
	maxBytes                                    int64
}

func newCheckStore(log *slog.Logger) *checkStore {
	return &checkStore{
		items: map[string]*checkItem{}, now: time.Now, log: log, slot: make(chan struct{}, 1),
		decode: decodeLabel, ttl: checkTTL, unpolled: 1500 * time.Millisecond, timeout: 2 * time.Second,
		pollWait: 8 * time.Second, heldAfter: 10 * time.Second, maxItems: checkMaxItems, maxBytes: checkBudget,
	}
}

// put holds labels (not copies) under a new token whose context derives from
// parent. ok is false, and nothing is held, when there's nothing to check or
// the labels alone are over the budget. Otherwise the oldest tokens are
// dropped, their work cancelled, until the new one fits.
func (st *checkStore) put(parent context.Context, labels []checkInput) (string, bool) {
	var size int64
	for _, l := range labels {
		size += int64(len(l.PNG))
	}
	if len(labels) == 0 || size > st.maxBytes {
		return "", false
	}
	token := rand.Text()
	ctx, cancel := context.WithCancel(parent)
	it := &checkItem{token: token, ctx: ctx, cancel: cancel, bytes: size, done: make(chan struct{})}
	for _, l := range labels {
		it.pngs = append(it.pngs, l.PNG)
		it.labels = append(it.labels, checkLabel{Page: l.Page, Index: l.Index, labelCheck: labelCheck{Status: checkPending}})
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	it.added = now
	st.sweepLocked(now)
	for len(st.items) > 0 && (len(st.items) >= st.maxItems || st.bytes+size > st.maxBytes) {
		st.dropLocked(st.oldestLocked())
	}
	st.items[token] = it
	st.bytes += size
	return token, true
}

// poll answers once every label is done, or after pollWait, or when the
// request goes away. The first poll starts the decode. ok is false for a
// token that's unknown, expired or cancelled.
func (st *checkStore) poll(ctx context.Context, token string) (checkReply, bool) {
	st.mu.Lock()
	it, ok := st.items[token]
	if !ok || st.expired(it, st.now()) {
		if ok {
			st.dropLocked(token)
		}
		st.mu.Unlock()
		return checkReply{}, false
	}
	it.open++
	if it.idle != nil {
		it.idle.Stop()
		it.idle = nil
	}
	if !it.started {
		it.started = true
		go st.run(it)
	}
	st.mu.Unlock()

	wait := time.NewTimer(st.pollWait)
	defer wait.Stop()
	select {
	case <-it.done:
	case <-wait.C:
	case <-ctx.Done():
	case <-it.ctx.Done():
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	it.open--
	it.lastEnd = st.now()
	if st.items[token] != it { // dropped while we waited
		return checkReply{}, false
	}
	reply := checkReply{Labels: append([]checkLabel(nil), it.labels...)}
	select {
	case <-it.done:
		reply.Done = true
	default:
		if it.open == 0 {
			it.idle = time.AfterFunc(st.unpolled, func() { st.cancelIfUnpolled(it) })
		}
	}
	return reply, true
}

// cancelIfUnpolled drops it when it's still unfinished and nobody has polled
// it for st.unpolled.
func (st *checkStore) cancelIfUnpolled(it *checkItem) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.items[it.token] != it || it.open > 0 || st.now().Sub(it.lastEnd) < st.unpolled {
		return
	}
	select {
	case <-it.done: // finished: its results stay readable for the TTL
	default:
		st.dropLocked(it.token)
	}
}

// run checks the token's labels in order, each in the decode slot, and
// releases each PNG once it has a result.
func (st *checkStore) run(it *checkItem) {
	start := time.Now()
	for i := range it.labels {
		st.mu.Lock()
		data := it.pngs[i]
		st.mu.Unlock()
		c, ok := st.checkOne(it.ctx, data)
		st.mu.Lock()
		if !ok || st.items[it.token] != it { // cancelled: a late result is thrown away
			st.mu.Unlock()
			return
		}
		it.labels[i].labelCheck = c
		it.pngs[i] = nil
		it.bytes -= int64(len(data))
		st.bytes -= int64(len(data))
		st.mu.Unlock()
	}
	st.logDone(it, time.Since(start)) // before done: a poll that sees done sees the log line too
	close(it.done)
}

// checkOne waits for the slot, then decodes data with a timeout that starts
// once the slot is taken. gozxing takes no context, so a decode that times
// out can't be stopped: it keeps the slot until it really returns, and if
// that's still not happened heldAfter later, that's logged, once. ok is
// false when ctx ended first.
func (st *checkStore) checkOne(ctx context.Context, data []byte) (labelCheck, bool) {
	select {
	case st.slot <- struct{}{}:
	case <-ctx.Done():
		return labelCheck{}, false
	}
	if ctx.Err() != nil { // both were ready: cancelled work never starts
		<-st.slot
		return labelCheck{}, false
	}
	result := make(chan labelCheck, 1)
	finished := make(chan struct{})
	go func() {
		defer func() { <-st.slot }() // held until the decode really returns
		defer close(finished)
		result <- checkLabelPNG(data, st.decode)
	}()
	timer := time.NewTimer(st.timeout)
	defer timer.Stop()
	select {
	case c := <-result:
		if c.panicked {
			st.log.Warn("check decode panicked") // never the value: it can quote the label
		}
		return c, true
	case <-timer.C:
		time.AfterFunc(st.heldAfter, func() {
			select {
			case <-finished:
			default:
				st.heldOnce.Do(func() { st.log.Warn("check slot held past timeout") })
			}
		})
		return labelCheck{Status: checkTimedOut}, true
	case <-ctx.Done():
		return labelCheck{}, false
	}
}

// logDone logs counts only: never a value, a format or an error's text.
func (st *checkStore) logDone(it *checkItem, took time.Duration) {
	n := map[string]int{}
	for _, l := range it.labels {
		n[l.Status]++
	}
	st.log.Info("check", "token", it.token[:6], "labels", len(it.labels), "ok", n[checkOK], "reads", n[checkReads],
		"none", n[checkNone], "timeout", n[checkTimedOut], "error", n[checkError], "ms", took.Milliseconds())
}

func (st *checkStore) expired(it *checkItem, now time.Time) bool { return now.Sub(it.added) >= st.ttl }

func (st *checkStore) sweep() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked(st.now())
}

func (st *checkStore) janitor(ctx context.Context, every time.Duration) {
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

func (st *checkStore) sweepLocked(now time.Time) {
	for tok, it := range st.items {
		if st.expired(it, now) {
			st.dropLocked(tok)
		}
	}
}

func (st *checkStore) oldestLocked() string {
	var oldest string
	var at time.Time
	for tok, it := range st.items {
		if oldest == "" || it.added.Before(at) {
			oldest, at = tok, it.added
		}
	}
	return oldest
}

// dropLocked forgets a token, cancels its work and releases its PNGs.
func (st *checkStore) dropLocked(token string) {
	it, ok := st.items[token]
	if !ok {
		return
	}
	delete(st.items, token)
	it.cancel()
	if it.idle != nil {
		it.idle.Stop()
	}
	st.bytes -= it.bytes
	it.bytes = 0
	clear(it.pngs)
}

func (st *checkStore) len() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.items)
}

func (st *checkStore) size() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.bytes
}

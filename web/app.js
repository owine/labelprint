// labelprint front end. No framework, no build step: everything the server
// sends that came from a file or from lprint goes into the DOM as text.
'use strict';

const $ = (id) => document.getElementById(id);
const el = (tag, props = {}, ...children) => {
  const node = Object.assign(document.createElement(tag), props);
  for (const c of children) node.append(c);
  return node;
};

// What the whole page shares. Each file is an entry (newEntry); state is the
// open one, the file in the editor.
const app = {
  queue: [],           // the entries, in order
  mine: new Set(),     // job ids submitted from this page
  lastResult: null,    // { jobs, warnings } of the last print, for live updates
  maxMB: 20,
  maxCopies: 100,
  defaultCopies: '1',  // the remembered copies count (#13): new entries start from it
  printing: false,     // print all is running: the queue is locked
  bg: null,            // the background scheduler's one request: { e, what: preview | check }
  get open() { return state; },
};

let nextEntryId = 0;
const blankPdf = () => ({ pages: new Map(), media: null, labelsElsewhere: 0, meta: null });

// One file: its own settings, preview, edits and barcode check. The timer,
// the two AbortControllers and previewSeq are the entry's own, so a reply or
// a debounce always lands on the file it was for, open or not.
function newEntry(file) {
  return {
    id: ++nextEntryId,   // for DOM and focus keys
    file,
    kind: null,          // server-confirmed: zpl | pdf | png | jpeg | gif
    previewSeq: 0,       // drops out-of-order preview replies
    layout: 'auto',      // label area for pdfs and images: auto | page
    pdf: blankPdf(),
    previewError: null,  // a field the last preview refused (pages, crops, layout): print waits for a good one
    check: null,         // its barcode check: { token, labels: Map "page:index" → result, failed, done }
    copies: app.defaultCopies, // as typed
    pages: '',           // the page range, as typed
    reply: null,         // the last applied preview reply: zpl and image redraws come from it
    error: null,         // { field, message } of the last refused preview
    pending: !!file,     // a preview is wanted that hasn't been applied yet
    timer: null,         // its preview debounce
    abort: null,         // AbortController of its preview request in flight
    checkAbort: null,    // AbortController of its barcode-check poll
    status: file ? 'previewing' : null, // previewing | ready | failed | print-failed | partly-sent
    sent: [],            // job ids sent before a print failed partway
    held: null,          // { seq, r, err }: a reply that landed mid-drag
    checkRetried: false, // a 404'd check has had its one re-preview
  };
}

// With no file open, state is the blank entry: never null, so no listener has
// to check.
const blank = newEntry(null);
let state = blank;

// ---------------------------------------------------------------- api

async function api(path, opts = {}) {
  let res;
  try {
    res = await fetch(path, opts);
  } catch {
    throw new Error("can't reach labelprint — is the container up?");
  }
  let body = null;
  try { body = await res.json(); } catch { /* not json */ }
  if (!res.ok) {
    const err = new Error(body?.error || `${res.status} ${res.statusText}`.trim());
    err.body = body;
    err.status = res.status; // a check's 404 (expired) is retried; other failures aren't
    throw err;
  }
  return body;
}

function formData(e, extra = {}) {
  const fd = new FormData();
  for (const [k, v] of Object.entries(extra)) fd.append(k, v);
  fd.append('file', e.file, e.file.name); // file last: fields first keeps the server's reads small
  return fd;
}

// ---------------------------------------------------------------- pdf label areas
//
// For PDFs the server finds label boxes on each page. The page shows them over
// the page image; edits go back as `crops` for that page. Boxes are fractions
// of the page as displayed: x, y, w, h from the top-left. An image (png, jpeg,
// gif) is a one-page pdf to the server, so it gets all of this as page 1.

// paged: kinds the server previews as pages with label boxes.
const paged = (kind) => kind === 'pdf' || kind === 'png' || kind === 'jpeg' || kind === 'gif';
// How the preview names a page: a pdf's by number, an image as itself.
const isImage = (e = state) => paged(e.kind) && e.kind !== 'pdf';
const pageRef = (pg) => (isImage() ? 'the image' : `page ${pg.page}`);
const pageTag = (pg) => (isImage() ? 'image' : `p${pg.page}`);
const wholeName = () => (isImage() ? 'image' : 'page');

const MIN_IN = 0.5;   // the server refuses smaller boxes
const MAX_BOXES = 8;  // per page, as the server allows
const NUDGE_IN = 0.05;     // arrow keys move or resize a box this far
const NUDGE_BIG_IN = 0.25; // with shift
const round4 = (v) => Math.round(v * 1e4) / 1e4;
const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
const fmtIn = (v) => String(Math.round(v * 100) / 100);
const arrBox = (a) => ({ x: a[0], y: a[1], w: a[2], h: a[3] });
const cloneBoxes = (bs) => bs.map((b) => ({ ...b, tab: b.tab ? { ...b.tab } : null }));

// A reply already on its way was asked for with the pages being dropped, so
// it's dropped too (preview's seq check) rather than bring them back.
function clearEdits(e) {
  e.pdf.pages = new Map();
  e.previewSeq++;
  stopCheck(e);
}

const unionBox = (a, b) => {
  const x = Math.min(a.x, b.x), y = Math.min(a.y, b.y);
  return { x, y, w: Math.max(a.x + a.w, b.x + b.w) - x, h: Math.max(a.y + a.h, b.y + b.h) - y };
};

// growBox is b grown to at least MIN_IN each side, centred on it and kept on
// the page, so the server accepts a doc tab printed on its own.
function growBox(b, pg) {
  const w = Math.min(1, Math.max(b.w, MIN_IN / pg.widthIn));
  const h = Math.min(1, Math.max(b.h, MIN_IN / pg.heightIn));
  return { x: clamp(b.x + b.w / 2 - w / 2, 0, 1 - w), y: clamp(b.y + b.h / 2 - h / 2, 0, 1 - h), w, h };
}

const separate = (b) => !!b.tab && b.choice === 'separate';

// flatBoxes is what a page prints, in order: each box, its doc tab as chosen
// (included in it, or right after it as its own label). A skipped page (no
// label found while other pages have one) prints nothing until it's included.
// Each carries its box's flip, so a doc tab printed separately turns with its
// label.
function flatBoxes(pg) {
  if (pg.skipped && !pg.edited) return [];
  const out = [];
  for (const b of pg.boxes) {
    const flip = !!b.flip;
    const a = b.tab && b.choice === 'include' ? unionBox(b, b.tab) : b;
    out.push({ x: a.x, y: a.y, w: a.w, h: a.h, flip });
    if (separate(b)) out.push({ ...growBox(b.tab, pg), flip });
  }
  return out;
}

// labelNumbers gives each box the number its label prints as (a doc tab
// printed separately takes the next one), so the badge on the page and the
// caption beside it agree.
function labelNumbers(pg) {
  let k = 0;
  return pg.boxes.map((b) => {
    const n = ++k;
    if (separate(b)) k++;
    return n;
  });
}

// A detected or fallback box can be under MIN_IN (a skipped page holding one
// line of text, a small label's box once its page is edited): each box is
// grown to it before sending, since the server refuses smaller crops. A
// flipped box gets a fifth number, 1, and the server turns its label 180°.
function cropsField(e) {
  const out = {};
  for (const pg of e.pdf.pages.values()) {
    if (!pg.edited || !pg.boxes.length) continue;
    out[pg.page] = flatBoxes(pg).map((b) => {
      const g = growBox(b, pg);
      const a = [g.x, g.y, g.w, g.h].map(round4);
      return b.flip ? [...a, 1] : a;
    });
  }
  return Object.keys(out).length ? JSON.stringify(out) : '';
}

// requestFields are the form fields both preview and print send for an
// entry: its own page range, layout and boxes, and the shared label size.
// #pages and #copies only mirror the open entry, so they're never read here.
function requestFields(e, media = $('media').value) {
  const f = { pages: e.pages, media, layout: e.layout };
  const crops = cropsField(e);
  if (crops) f.crops = crops;
  return f;
}

// absorbPdf takes a preview reply, keeping the boxes of pages the user edited.
// An edited page also keeps what detection said about it (auto, fallback,
// skipped), since the reply describes the crops sent; reset goes back to that.
function absorbPdf(e, r) {
  e.pdf.media = r.widthIn ? { w: r.widthIn, h: r.heightIn } : null;
  e.pdf.meta = { selected: r.selected, pageCount: r.pageCount };
  const old = e.pdf.pages;
  const next = new Map();
  let shownLabels = 0;
  for (const p of r.pages || []) {
    const fresh = (p.boxes || []).map((a, i) => ({
      ...arrBox(a), autoIndex: i, tab: p.tabs?.[i] ? arrBox(p.tabs[i]) : null, choice: 'off',
      tabScale: p.tabScales?.[i] || null, // the server's scale for "include"
    }));
    const prev = old.get(p.page);
    const edited = !!prev?.edited;
    const pg = {
      page: p.page, image: p.image, widthIn: p.widthIn, heightIn: p.heightIn,
      auto: edited ? prev.auto : !!p.auto,
      fallback: edited ? prev.fallback : !!p.fallback,
      skipped: edited ? prev.skipped : !!p.skipped,
      labelImages: p.labelImages || [],
      edited,
      boxes: edited ? prev.boxes : fresh,
      autoBoxes: edited ? prev.autoBoxes : cloneBoxes(fresh),
    };
    if (!p.skipped) shownLabels += fresh.length;
    next.set(p.page, pg);
  }
  // labels on pages past the preview cap, which the page doesn't hold
  e.pdf.labelsElsewhere = (r.labels || 0) - shownLabels;
  e.pdf.pages = next;
}

function pdfLabelCount(e) {
  let n = e.pdf.labelsElsewhere;
  for (const pg of e.pdf.pages.values()) n += flatBoxes(pg).length;
  return n;
}

// Pages the user left with no boxes. A skipped page isn't one: it prints
// nothing on purpose.
function emptyPages(e) {
  return [...e.pdf.pages.values()].filter((pg) => pg.edited && !pg.boxes.length);
}

// Whether pg is the page on screen. Changing the page range (or size, or
// layout) drops the pages before the reply that replaces them lands, so until
// then the boxes on screen belong to no page and edits to them are ignored.
// A closed entry's pages never match: state is the open entry.
const live = (pg) => state.pdf.pages.get(pg.page) === pg;

// An edit to a page: draw it, recount, and re-preview with its crops.
// Until the new reply lands the page's label images are the old boxes', so
// they're shown dimmed and captioned as updating (absorbPdf makes pages anew).
// A page left with no boxes isn't sent, so there's nothing to re-preview.
function markEdited(pg) {
  pg.edited = true;
  pg.stale = true;
  renderPdf();
  updateSummary();
  if (pg.boxes.length) schedulePreview(state);
}

// A moved or resized box no longer matches its detected doc tab: its choice
// goes, and the tab stays outlined (left off) until the page is reset.
function boxChanged(pg, b) {
  if (b.tab) {
    b.oldTab = b.tab;
    b.tab = null;
    b.choice = 'off';
  }
  markEdited(pg);
}

// Focus moves to the box that took the removed one's place, or to "add label"
// when none is left, so the keyboard isn't dropped back at the top.
function removeBox(pg, b) {
  if (!live(pg)) return;
  const i = pg.boxes.indexOf(b);
  pg.boxes = pg.boxes.filter((x) => x !== b);
  markEdited(pg);
  if (!focusBox(pg, Math.min(i, pg.boxes.length - 1))) focusTool(pg, 'add');
}

// A centred box the size of the label, turned to the page's orientation and
// shrunk to fit it; half the page each way when the label size is unknown.
// On a skipped page the whole-page box wasn't printing, so the new box
// replaces it rather than joining it.
function addBox(pg) {
  if (!live(pg)) return;
  const m = state.pdf.media;
  let w = 0.5, h = 0.5;
  if (m) {
    let mw = m.w, mh = m.h;
    if ((pg.widthIn > pg.heightIn) !== (mw > mh)) [mw, mh] = [mh, mw];
    const fit = Math.min(1, pg.widthIn / mw, pg.heightIn / mh);
    w = (mw * fit) / pg.widthIn;
    h = (mh * fit) / pg.heightIn;
  }
  if (pg.skipped && !pg.edited) pg.boxes = [];
  pg.boxes.push({ x: (1 - w) / 2, y: (1 - h) / 2, w, h, autoIndex: null, tab: null, choice: 'off' });
  markEdited(pg);
  focusBox(pg, pg.boxes.length - 1);
}

// The doc tab's choice: leave it off, include it in its label's box, or print
// it right after as its own label. It belongs to the box object, so other
// boxes keep theirs when a tab box is inserted.
function setChoice(pg, b, choice) {
  if (b.choice === choice || !live(pg)) return;
  b.choice = choice;
  markEdited(pg);
}

// Drop a page's edits and go back to what detection found.
function resetPage(pg) {
  if (!live(pg)) return;
  pg.edited = false;
  pg.stale = true;
  pg.boxes = cloneBoxes(pg.autoBoxes);
  renderPdf();
  updateSummary();
  schedulePreview(state);
}

// ---------------------------------------------------------------- printer

const terminal = new Set(['completed', 'canceled', 'aborted']);
const reasonText = (r) => r.replace(/-(report|warning|error)$/, '').replaceAll('-', ' ');

async function loadPrinter() {
  let p;
  try {
    p = await api('/api/printer');
  } catch (e) {
    renderPrinter({ reachable: false, error: e.message, queue: 'lprint' });
    return null;
  }
  renderPrinter(p);
  return p;
}

function renderPrinter(p) {
  const dot = $('printer-dot');
  const badges = $('printer-badges');
  badges.replaceChildren();
  $('printer-name').textContent = p.queue || 'lprint';
  $('printer').title = p.uri || '';
  if (!p.reachable) {
    dot.className = 'printer-dot bad';
    badges.append(el('span', { className: 'badge badge-danger', textContent: 'unreachable', title: p.error || '' }));
    return;
  }
  const stopped = p.state === 'stopped';
  dot.className = 'printer-dot ' + (stopped ? 'bad' : p.reasons?.length ? 'warn' : 'ok');
  badges.append(el('span', {
    className: 'badge ' + (stopped ? 'badge-danger' : 'badge-neutral'),
    textContent: p.state, title: p.message || '',
  }));
  for (const r of p.reasons || []) {
    badges.append(el('span', { className: 'badge badge-warning', textContent: reasonText(r), title: r }));
  }
}

function fillMedia(p) {
  const sel = $('media');
  const keep = mediaChoice || pref.get(PREF_MEDIA) || sel.value;
  sel.replaceChildren();
  for (const m of p.media) {
    sel.append(el('option', { value: m.name, textContent: m.label + (m.ready ? ' · loaded' : '') }));
  }
  sel.value = keep && p.media.some((m) => m.name === keep) ? keep : p.defaultMedia;
  if (!sel.value && sel.options.length) sel.selectedIndex = 0;
}

// ---------------------------------------------------------------- file choice

const fmtSize = (n) => (n < 1024 ? `${n} B` : n < 1 << 20 ? `${(n / 1024).toFixed(0)} KB` : `${(n / (1 << 20)).toFixed(1)} MB`);

// ---------------------------------------------------------------- the queue
//
// Files are added, never replaced, up to MAX_QUEUE. Each is previewed in the
// background (kick); the open one (state) is in the editor below the list.

const MAX_QUEUE = 10;

// say puts a message in the status line by the list: adding files, previews
// refused. #result is for print results only.
function say(text) {
  $('queue-status').textContent = text;
}

const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;

// addFiles adds files to the end of the queue. Zero-byte files (folders in a
// drop) are skipped and a file over the size limit is added as failed;
// there's no type check here, the server's preview is it. The first file into
// an empty queue opens, and so does the file a paste adds (open); other
// additions leave the open file where it is.
function addFiles(list, { open = false } = {}) {
  const all = [...list];
  if (!all.length) return; // a cancelled picker
  const files = all.filter((f) => f.size > 0);
  const msgs = [];
  const skipped = all.length - files.length;
  if (skipped) msgs.push(`skipped ${plural(skipped, 'empty file')} or folder${skipped === 1 ? '' : 's'}`);
  const take = files.slice(0, Math.max(0, MAX_QUEUE - app.queue.length));
  const left = files.length - take.length;
  if (left) msgs.push(`the queue holds ${MAX_QUEUE} files — ${left} ${left === 1 ? "wasn't" : "weren't"} added`);
  const added = take.map((f) => {
    const e = newEntry(f);
    if (f.size > app.maxMB * (1 << 20)) {
      e.pending = false;
      e.status = 'failed';
      e.error = { field: null, message: `over the ${app.maxMB} MB limit` };
      msgs.push(`${f.name} is ${fmtSize(f.size)} — the limit is ${app.maxMB} MB`);
    }
    app.queue.push(e);
    return e;
  });
  say(msgs.join(' · '));
  if (added.length && (open || state === blank)) openEntry(added[0]);
  renderQueue();
  kick();
}

// dropEntry stops everything an entry has running: its debounce, its preview
// request and its check poll, which frees the server's slot.
function dropEntry(e) {
  clearTimeout(e.timer);
  e.timer = null;
  e.abort?.abort();
  e.abort = null;
  e.checkAbort?.abort();
  e.checkAbort = null;
  if (app.bg?.e === e) app.bg = null;
}

// Removing a file opens the one that takes its place when it was open, and
// moves focus to the next row's name (the previous row's if it was last), or
// to the file input once the queue is empty.
function removeEntry(e) {
  const i = app.queue.indexOf(e);
  if (i < 0) return;
  dropEntry(e);
  app.queue.splice(i, 1);
  if ($('queue-status').textContent.startsWith(`${e.file.name}:`)) say(''); // its refusal
  const next = app.queue[i] || app.queue[i - 1] || null;
  if (e === state) openEntry(next || blank);
  renderQueue();
  kick();
  if (next) rows.get(next.id).open.focus();
  else $('file').focus();
}

function clearQueue() {
  for (const e of app.queue) dropEntry(e);
  app.queue = [];
  openEntry(blank);
  renderQueue();
  say('');
  $('file').focus();
}

// openEntry puts e in the editor: the mirrors (#copies, #pages, the layout
// radios) are written from it, and it's drawn from its own data. Opening
// never loses a pending preview: the entry left keeps its debounce and its
// request in flight. A preview e is waiting for that nothing is sending yet
// goes now, and a check it had paused picks up again.
//
// Each check poll has one owner, the open entry or the scheduler. Opening an
// entry the scheduler is busy with takes its request over (the scheduler is
// free for the next); the entry left hands an unfinished check back to the
// scheduler, which keeps polling it if it's idle. Otherwise the poll pauses:
// its results so far stay in prev.check, and the scheduler resumes it later.
function openEntry(e) {
  if (e === state) return;
  const prev = state;
  state = e;
  if (e === blank) blank.copies = app.defaultCopies;
  if (app.bg?.e === e) app.bg = null; // the open entry's requests aren't the scheduler's
  if (prev.checkAbort) {
    if (!app.bg) {
      app.bg = { e: prev, what: 'check' };
    } else {
      prev.checkAbort.abort();
      prev.checkAbort = null;
    }
  }
  $('copies').value = e.copies;
  $('pages').value = e.pages;
  document.querySelector(`input[name="layout"][value="${e.layout}"]`).checked = true;
  if (needsPreview(e)) requestPreview(e);
  else if (e.check && !e.check.done && !e.checkAbort) pollCheck(e);
  renderEntry(e);
  renderQueue();
  kick();
}

// The background scheduler sends one request at a time for files that aren't
// open, in queue order, so the server's two pipeline slots aren't flooded
// (the open file's own requests use the other, and never wait for it):
// previews first, then barcode checks. A check token that's never been polled
// waits on the server for up to 10 minutes, so checks can wait their turn.
function kick() {
  if (app.bg) return;
  const p = app.queue.find((x) => x !== state && needsPreview(x));
  if (p) {
    app.bg = { e: p, what: 'preview' };
    requestPreview(p);
    return;
  }
  const c = app.queue.find((x) => x !== state && x.check && !x.check.done && !x.checkAbort);
  if (c) {
    app.bg = { e: c, what: 'check' };
    pollCheck(c);
  }
}

// release frees the scheduler once e's background request (what) is over,
// and lets it pick the next.
function release(e, what) {
  if (app.bg?.e === e && app.bg.what === what) app.bg = null;
  setTimeout(kick);
}

// ---------------------------------------------------------------- the list

const rows = new Map(); // entry id → its row's elements, kept so focus survives updates

// labelUnits: what one copy of an entry prints. A zpl file counts as one, as
// the summary always has: it's one job.
const labelUnits = (e) => (paged(e.kind) ? pdfLabelCount(e) : e.kind === 'zpl' ? 1 : 0);
const copiesOK = (e) => {
  const n = wholeCopies(e.copies);
  return n >= 1 && n <= app.maxCopies;
};

// Whether any label the entry would print shrinks below SCALE_WARN.
function shrinks(e) {
  for (const pg of e.pdf.pages.values()) {
    if (!pg.stale && pg.labelImages.some((li) => li.scale < SCALE_WARN)) return true;
  }
  return false;
}

// The reasons a row gets a ⚠, in words.
function rowWarnings(e) {
  const out = [];
  if (e.status === 'failed') out.push(e.error?.message || 'refused');
  if (e.previewError) out.push(e.previewError);
  if (!copiesOK(e)) out.push(`copies must be a whole number, 1 to ${app.maxCopies}`);
  if (paged(e.kind)) {
    const empty = emptyPages(e);
    if (empty.length) out.push(isImage(e) ? 'the image has no labels' : `page ${empty[0].page} has no labels`);
    if (shrinks(e)) out.push('shrinks below 95%');
    if (unreadLabels(e)) out.push("barcode couldn't be read");
  }
  if (e.kind === 'zpl' && e.reply?.fitWarning) out.push('zpl bigger than the labels');
  return out;
}

function makeRow(e) {
  const open = el('button', { type: 'button', className: 'queue-open', textContent: e.file.name });
  open.setAttribute('aria-controls', 'editor');
  open.addEventListener('click', () => openEntry(e));
  const remove = el('button', { type: 'button', className: 'btn btn-ghost btn-sm', textContent: 'remove' });
  remove.setAttribute('aria-label', `remove ${e.file.name}`);
  remove.addEventListener('click', () => removeEntry(e));
  const r = {
    li: el('li', { className: 'queue-row' }),
    open,
    kind: el('span'),
    count: el('span', { className: 'queue-meta' }),
    warn: el('span', { className: 'queue-warn' }),
    copies: el('span', { className: 'queue-meta' }),
    check: el('span', { className: 'queue-meta' }),
    remove,
  };
  r.li.append(r.open, r.kind, r.count, r.warn, r.copies, r.check, r.remove);
  return r;
}

// Row values come from the entry each time; there's no cache. Updates from
// background work aren't announced: the list isn't a live region.
function fillRow(e, r) {
  const isOpen = e === state;
  r.li.classList.toggle('open', isOpen);
  if (isOpen) r.open.setAttribute('aria-current', 'true');
  else r.open.removeAttribute('aria-current');
  const failed = e.status === 'failed';
  r.kind.className = 'badge ' + (failed ? 'badge-danger' : 'badge-accent');
  r.kind.textContent = e.kind || (failed ? 'rejected' : '…');
  r.count.classList.toggle('bad', failed || e.status === 'partly-sent');
  r.count.textContent = failed ? 'failed'
    : e.status === 'partly-sent' ? 'partly sent'
      : !e.reply ? (e.previewError && !busy(e) ? 'not previewed' : 'previewing…')
        : plural(labelUnits(e), 'label') + (e.status === 'print-failed' ? ' · not sent' : '');
  const warns = rowWarnings(e);
  r.warn.hidden = !warns.length;
  r.warn.title = warns.join('; ');
  r.warn.replaceChildren('⚠', el('span', { className: 'visually-hidden', textContent: ` ${warns.join('; ')}` }));
  r.copies.textContent = `×${e.copies}`;
  r.check.textContent = checkStatus(e);
  r.check.hidden = !r.check.textContent;
}

// A row's barcode check: running, waiting for the scheduler, or given up. A
// label whose barcode couldn't be read is the row's ⚠.
function checkStatus(e) {
  if (!paged(e.kind) || !e.check) return '';
  if (e.checkAbort) return 'checking…';
  if (!e.check.done) return 'barcode not checked';
  if (e.check.failed) return "barcode check didn't finish";
  return '';
}

function renderQueue() {
  const list = $('queue-list');
  const ids = new Set(app.queue.map((e) => e.id));
  for (const [id, r] of rows) {
    if (!ids.has(id)) {
      r.li.remove();
      rows.delete(id);
    }
  }
  let labels = 0;
  for (const e of app.queue) {
    let r = rows.get(e.id);
    if (!r) {
      r = makeRow(e);
      rows.set(e.id, r);
      list.append(r.li);
    }
    fillRow(e, r);
    if (e.reply && e.status !== 'failed' && copiesOK(e)) labels += labelUnits(e) * wholeCopies(e.copies);
  }
  const n = app.queue.length;
  $('queue').hidden = !n;
  if (!n) disarmClear();
  $('queue-title').textContent = `queue · ${plural(n, 'file')} · ${plural(labels, 'label')}`;
}

// clear all takes two presses: "clear all", then "clear all — sure?" within
// 4 s, like cancelling a job.
let clearTimer = null;
function disarmClear() {
  clearTimeout(clearTimer);
  clearTimer = null;
  const btn = $('clear-all');
  btn.textContent = 'clear all';
  btn.removeAttribute('aria-label');
  btn.classList.remove('armed');
}
$('clear-all').addEventListener('click', () => {
  const btn = $('clear-all');
  if (!clearTimer) {
    btn.textContent = 'clear all — sure?';
    btn.setAttribute('aria-label', 'clear all — sure?');
    btn.classList.add('armed');
    clearTimer = setTimeout(disarmClear, 4000);
    return;
  }
  disarmClear();
  clearQueue();
});

// For ZPL the size only shapes the preview (where the zpl doesn't set ^PW/^LL
// itself); the printer goes by the zpl, so it isn't sent with the job.
function setMediaHint(kind) {
  $('media-hint').textContent = kind === 'zpl' ? 'for the preview and the size check — the zpl sets the printed size' : '';
}

// ---------------------------------------------------------------- preview
//
// A preview goes in three steps. requestPreview sends one for an entry;
// applyPreview takes the reply into that entry's data, open or not; and
// renderEntry draws the open entry from its data. The entry is captured
// before any await, so a reply always lands on the file it was asked for.

// busy: a preview is wanted or on its way.
const busy = (e) => e.pending || !!e.abort;
// needsPreview: wanted, and nothing (a debounce, a request) is sending it yet.
const needsPreview = (e) => !!e.file && e.pending && !e.timer && !e.abort;

async function requestPreview(e) {
  if (!e.file) return;
  clearTimeout(e.timer);
  e.timer = null;
  const seq = ++e.previewSeq;
  e.abort?.abort();
  const ctrl = new AbortController();
  e.abort = ctrl;
  stopCheck(e);
  if (e === state) {
    $('preview').classList.add('loading');
    $('pages').removeAttribute('aria-invalid');
  }
  let r = null;
  let err = null;
  try {
    r = await api('/api/preview', { method: 'POST', body: formData(e, requestFields(e)), signal: ctrl.signal });
  } catch (x) {
    err = x;
  }
  if (e.abort === ctrl) e.abort = null;
  if (!ctrl.signal.aborted) applyPreview(e, seq, r, err); // else a newer request, or the entry was dropped
  if (!e.abort) release(e, 'preview');
}

// applyPreview takes a reply (r) or a refusal (err) into the entry's data.
// A reply that landed mid-drag on the open entry is held until the drag ends
// (startDrag applies it), before absorbPdf: that makes every page anew, so
// the page and box the drag holds would edit nothing and the box would snap
// back.
function applyPreview(e, seq, r, err) {
  if (seq !== e.previewSeq || !app.queue.includes(e)) return;
  if (e === state && dragging) {
    e.held = { seq, r, err };
    return;
  }
  e.held = null;
  e.pending = false;
  if (err) {
    const field = err.body?.field || null;
    e.error = { field, message: err.message };
    if (field) {
      e.previewError = err.message;
    } else {
      e.kind = null;
      e.status = 'failed';
    }
  } else {
    e.error = null;
    e.previewError = null;
    e.kind = r.kind;
    e.reply = r;
    if (paged(r.kind)) absorbPdf(e, r);
    if (e.status === 'previewing' || e.status === 'failed') e.status = 'ready';
    startCheck(e, r.check); // before drawing, so the label lines start as "checking"
  }
  renderEntry(e);
  renderQueue();
  if (err && !err.body?.field) say(`${e.file.name}: ${err.message}`);
}

// renderEntry draws the open entry from its data: the file row, the fields
// its kind has, and its preview. The pages field and label area stay through
// a refused re-preview (they come from the last reply), so the range or
// layout that caused it can be changed.
function renderEntry(e) {
  if (e !== state) return;
  const box = $('preview');
  $('drop').classList.toggle('has-file', app.queue.length > 0);
  $('file-row').hidden = !e.file;
  if (!e.file) {
    box.hidden = true;
    $('pages-field').hidden = true;
    $('layout-field').hidden = true;
    setMediaHint(null);
    updateSummary();
    return;
  }
  $('file-name').textContent = e.file.name;
  $('file-size').textContent = fmtSize(e.file.size);
  const kind = $('file-kind');
  const rejected = e.status === 'failed';
  kind.textContent = e.kind || (rejected ? 'rejected' : '…');
  kind.className = 'badge ' + (rejected ? 'badge-danger' : 'badge-accent');
  const k = e.reply?.kind;
  // an image is one page; a range refused on the first preview (a pdf over the
  // page limit) needs the field to fix it
  $('pages-field').hidden = k !== 'pdf' && e.error?.field !== 'pages';
  $('layout-field').hidden = !paged(k);
  $('layout-whole').textContent = k === 'pdf' ? 'whole page' : 'whole image';
  setMediaHint(k);
  const pagesErr = e.error?.field === 'pages';
  if (pagesErr) $('pages').setAttribute('aria-invalid', 'true');
  else $('pages').removeAttribute('aria-invalid');
  $('pages-hint').textContent = pagesErr ? e.error.message : 'e.g. 1-3, 5';
  box.hidden = !e.reply || rejected;
  if (box.hidden) {
    // nothing to draw
  } else if (paged(e.reply.kind) && !e.pdf.pages.size && busy(e)) {
    renderUpdating();
  } else {
    renderPreview(e);
    if (e.error?.field) fieldError(e);
  }
  box.classList.toggle('loading', !!e.abort || (e.pending && !e.pdf.pages.size));
  updateSummary();
}

// A refused range, size or layout: the pages on screen are gone from the
// entry, so they're taken away rather than left as boxes that edit nothing.
// A refused box or layout is said under the preview; a refused range by its
// field (renderEntry).
function fieldError(e) {
  const note = $('preview-note');
  if (paged(e.kind) && !e.pdf.pages.size) {
    $('preview-grid').replaceChildren();
    note.textContent = '';
  }
  if (e.error.field === 'crops' || e.error.field === 'layout') {
    note.textContent = e.error.message;
    note.classList.add('err');
  }
}

// An entry opened while its range, size or layout is being re-previewed has
// no pages to show until the reply lands (clearEdits dropped them).
function renderUpdating() {
  $('preview-grid').replaceChildren();
  const note = $('preview-note');
  note.textContent = 'updating…';
  note.classList.remove('err');
  $('image-note').hidden = true;
  $('preview-warning').hidden = true;
  document.querySelector('.zpl-source')?.remove();
  document.querySelector('.zpl-tracking')?.remove();
}

function renderPreview(e) {
  const r = e.reply;
  const grid = $('preview-grid');
  const note = $('preview-note');
  if (!paged(r.kind)) grid.replaceChildren(); // renderPdf swaps the pages itself, keeping focus
  grid.classList.toggle('pdf-mode', paged(r.kind));
  note.classList.remove('err');
  imageNote(r);
  // zpl only, and never a block: the zpl is sent unchanged whatever its size
  const warning = $('preview-warning');
  warning.textContent = r.fitWarning || '';
  warning.hidden = !r.fitWarning;
  document.querySelector('.zpl-source')?.remove();
  document.querySelector('.zpl-tracking')?.remove();
  if (paged(r.kind)) {
    renderPdf();
  } else if (r.kind === 'zpl') {
    const n = r.labels || 0;
    const count = `${n} label${n === 1 ? '' : 's'}`;
    if (r.previewError) {
      note.textContent = `${count} · no preview: ${r.previewError}. printing still sends the zpl unchanged.`;
    } else {
      const shown = r.pages.length;
      note.textContent = `${count} · drawn here at ${r.widthIn} × ${r.heightIn} in, an emulation of the printer ` +
        `— it gets the original zpl` + (n > shown ? ` · showing the first ${shown}` : '');
      for (const p of r.pages) {
        grid.append(el('figure', { className: 'preview-item' },
          el('img', { src: p.image, alt: `label ${p.page}`, loading: 'lazy' }),
          el('figcaption', { textContent: `label ${p.page}` })));
      }
    }
    zplSource(e, !!r.previewError);
    zplTracking(r.tracking);
  }
}

const fmtPPI = (v) => String(Math.round(v * 10) / 10);

// An image's size, above its page: where its ppi came from, and the inches
// that makes it. A guessed ppi is a warning, so a wrong size can't go unseen;
// the boxes and "whole image" are there to act on it.
function imageNote(r) {
  const im = r.image;
  const node = $('image-note');
  node.hidden = !im;
  if (!im) return;
  const m = r.widthIn ? { w: r.widthIn, h: r.heightIn } : null;
  const inches = `${fmtIn(im.w / im.ppi)} × ${fmtIn(im.h / im.ppi)} in`;
  const px = `${im.w} × ${im.h} px`;
  const guessed = im.ppiSource === 'default';
  let how;
  if (guessed) how = `${px} (size guessed at ${fmtPPI(im.ppi)} ppi — check it)`;
  else if (im.ppiSource === 'phys' || im.ppiSource === 'jfif') how = `${px} at ${fmtPPI(im.ppi)} ppi (from the file)`;
  else how = `${px} at ${fmtPPI(im.ppi)} ppi (matched to the ${m ? `${fmtIn(m.w)} × ${fmtIn(m.h)} in ` : ''}labels)`;
  node.textContent = `${how} → ${inches}`;
  node.classList.toggle('warn', guessed);
}

function renderPdf() {
  const grid = $('preview-grid');
  const note = $('preview-note');
  const a = document.activeElement;
  const fig = a?.dataset?.tool ? a.closest('.pdf-page') : null;
  const focus = fig ? { page: fig.dataset.page, tool: a.dataset.tool } : null;
  const crop = a?.closest?.('.crop');
  const focusCrop = crop ? { page: crop.dataset.page, index: crop.dataset.index, del: a !== crop } : null;
  const { selected, pageCount } = state.pdf.meta;
  const shown = state.pdf.pages.size;
  const n = pdfLabelCount(state);
  const m = state.pdf.media;
  // an image's own line is the image note, above
  note.textContent = (isImage() ? '' : `${selected} of ${pageCount} page${pageCount === 1 ? '' : 's'} · `) +
    `${n} label${n === 1 ? '' : 's'}` +
    (m ? ` · black and white, sized for ${fmtIn(m.w)} × ${fmtIn(m.h)} in` : '') +
    (selected > shown ? ` · showing the first ${shown} pages` : '');
  grid.replaceChildren(...[...state.pdf.pages.values()].map(pageFigure));
  if (focus) focusTool({ page: focus.page }, focus.tool);
  if (focusCrop) focusBox({ page: focusCrop.page }, focusCrop.index, focusCrop.del);
}

function pageCaption(pg) {
  const n = pg.boxes.length;
  const tag = pageTag(pg);
  if (pg.edited) {
    const k = flatBoxes(pg).length;
    return `${tag} · ${k} label area${k === 1 ? '' : 's'}, edited`;
  }
  if (pg.skipped) return `${tag} · no label found, not printing`;
  if (pg.fallback) return `${tag} · no label found, using the whole ${wholeName()}`;
  if (pg.auto) return `${tag} · ${n} label${n === 1 ? '' : 's'} found`;
  return `${tag} · whole ${wholeName()}`;
}

function pageFigure(pg) {
  const skipped = pg.skipped && !pg.edited;
  const stage = el('div', { className: 'page-stage' },
    el('img', { src: pg.image, alt: pageRef(pg), draggable: false }));
  stage.style.aspectRatio = `${pg.widthIn} / ${pg.heightIn}`;
  const nums = labelNumbers(pg);
  pg.boxes.forEach((b, i) => {
    if (b.tab || b.oldTab) stage.append(tabOutline(pg, b, i, nums[i]));
    const node = boxEl(pg, b, i, nums[i]);
    if (skipped) node.classList.add('skipped');
    stage.append(node);
  });
  const tools = el('div', { className: 'page-tools' }, el('span', { className: 'caption', textContent: pageCaption(pg) }));
  if (skipped) {
    const include = el('button', { type: 'button', className: 'btn btn-ghost btn-sm', textContent: 'include' });
    include.setAttribute('aria-label', `include ${pageRef(pg)}: print it as a label`);
    include.dataset.tool = 'include';
    include.addEventListener('click', () => { if (!live(pg)) return; pg.edited = true; markEdited(pg); focusTool(pg, 'reset'); });
    tools.append(include);
  }
  const add = el('button', { type: 'button', className: 'btn btn-ghost btn-sm', textContent: 'add label' });
  add.setAttribute('aria-label', `add a label area to ${pageRef(pg)}`);
  add.dataset.tool = 'add';
  add.disabled = flatBoxes(pg).length >= MAX_BOXES;
  if (add.disabled) add.title = `${MAX_BOXES} label areas is the most ${isImage() ? 'an image' : 'a page'} can have`;
  add.addEventListener('click', () => addBox(pg));
  tools.append(add);
  if (pg.edited) {
    const reset = el('button', { type: 'button', className: 'btn btn-ghost btn-sm', textContent: 'reset' });
    reset.setAttribute('aria-label', `reset ${pageRef(pg)}`);
    reset.dataset.tool = 'reset';
    reset.addEventListener('click', () => { resetPage(pg); if (!focusTool(pg, 'include')) focusTool(pg, 'add'); });
    tools.append(reset);
  }
  if (!pg.boxes.length) tools.append(el('span', { className: 'badge badge-warning', textContent: 'no labels' }));
  const fig = el('figure', { className: 'pdf-page' },
    el('div', { className: 'page-col' }, stage, tools),
    el('div', { className: 'labels-col' }, ...labelFigures(pg)));
  fig.dataset.page = pg.page;
  return fig;
}

// The page is redrawn on every change, so a button that swaps for another
// (include ↔ reset) hands keyboard focus to its replacement.
function focusTool(pg, tool) {
  return focusEl($('preview-grid').querySelector(`.pdf-page[data-page="${pg.page}"] [data-tool="${tool}"]`));
}

// the box itself, or its × button
function focusBox(pg, i, del = false) {
  const sel = `.crop[data-page="${pg.page}"][data-index="${i}"]` + (del ? ' .crop-del' : '');
  return focusEl($('preview-grid').querySelector(sel));
}

function focusEl(node) {
  if (!node || node.disabled) return false;
  node.focus();
  return true;
}

function place(node, b) {
  Object.assign(node.style, { left: `${b.x * 100}%`, top: `${b.y * 100}%`, width: `${b.w * 100}%`, height: `${b.h * 100}%` });
}

// A box: drag it to move it, drag a corner to resize it, × to remove it.
// Focused, arrow keys nudge it, alt+arrow resizes it from the bottom-right
// corner, delete removes it. On a skipped page (shown, not printing) a move or
// resize includes the page, like "include" and then the edit; it has no × since
// there is nothing printing to take away.
function boxEl(pg, b, i, num) {
  const skipped = pg.skipped && !pg.edited;
  const node = el('div', { className: 'crop', tabIndex: 0 });
  node.dataset.page = pg.page;
  node.dataset.index = i;
  node.setAttribute('role', 'group');
  const size = `${fmtIn(b.w * pg.widthIn)} × ${fmtIn(b.h * pg.heightIn)} in`;
  const tab = !b.tab ? '' : `, doc tab ${{ off: 'left off', include: 'included', separate: `printed as label ${num + 1}` }[b.choice]}`;
  node.setAttribute('aria-label', skipped
    ? `label area ${num} on ${pageRef(pg)}, ${size}, not printing: move or resize it to include the ${wholeName()}`
    : `label area ${num} on ${pageRef(pg)}, ${size}${tab}: drag or arrow keys to move, ` +
      'corners or alt+arrow keys to resize, delete to remove');
  place(node, b);
  node.append(el('span', { className: 'crop-num', textContent: num }));
  for (const corner of ['nw', 'ne', 'sw', 'se']) {
    const h = el('span', { className: 'crop-handle' });
    h.dataset.corner = corner;
    node.append(h);
  }
  if (!skipped) {
    const del = el('button', { type: 'button', className: 'crop-del', textContent: '×' });
    del.setAttribute('aria-label', `remove label area ${num} from ${pageRef(pg)}`);
    del.addEventListener('pointerdown', (e) => e.stopPropagation());
    del.addEventListener('click', () => removeBox(pg, b));
    node.append(del);
  }
  node.addEventListener('pointerdown', (e) => startDrag(e, pg, b, node, e.target.dataset?.corner || null));
  node.addEventListener('keydown', (e) => keyBox(e, pg, b, node));
  return node;
}

// A preview reply that lands mid-drag is held (entry.held) until the drag
// ends: redrawing would swap the box being dragged for a new node and end the
// drag.
let dragging = false;

function startDrag(e, pg, b, node, corner) {
  if (e.button !== 0 || dragging || !live(pg)) return;
  e.preventDefault();
  node.focus();
  const entry = state;
  const stage = node.parentElement.getBoundingClientRect();
  const o = { ...b };
  const mw = MIN_IN / pg.widthIn;
  const mh = MIN_IN / pg.heightIn;
  let moved = false;
  dragging = true;
  node.setPointerCapture(e.pointerId);
  const move = (ev) => {
    if (ev.pointerId !== e.pointerId) return;
    moved ||= Math.abs(ev.clientX - e.clientX) + Math.abs(ev.clientY - e.clientY) > 2;
    if (!moved) return; // a click, not a drag
    const dx = (ev.clientX - e.clientX) / stage.width;
    const dy = (ev.clientY - e.clientY) / stage.height;
    if (!corner) {
      b.x = clamp(o.x + dx, 0, 1 - o.w);
      b.y = clamp(o.y + dy, 0, 1 - o.h);
    } else {
      let x0 = o.x, y0 = o.y, x1 = o.x + o.w, y1 = o.y + o.h;
      if (corner.includes('w')) x0 = clamp(x0 + dx, 0, x1 - mw); else x1 = clamp(x1 + dx, x0 + mw, 1);
      if (corner.includes('n')) y0 = clamp(y0 + dy, 0, y1 - mh); else y1 = clamp(y1 + dy, y0 + mh, 1);
      Object.assign(b, { x: x0, y: y0, w: x1 - x0, h: y1 - y0 });
    }
    place(node, b);
  };
  // pointerup commits; pointercancel (the browser took the pointer) puts the
  // box back; losing capture any other way commits what was dragged.
  const end = (ev) => {
    if (ev.pointerId !== e.pointerId) return;
    node.removeEventListener('pointermove', move);
    node.removeEventListener('pointerup', end);
    node.removeEventListener('pointercancel', end);
    node.removeEventListener('lostpointercapture', end);
    if (node.hasPointerCapture(e.pointerId)) node.releasePointerCapture(e.pointerId);
    if (ev.type === 'pointercancel' && moved) {
      Object.assign(b, o);
      place(node, b);
      moved = false;
    }
    dragging = false;
    const held = entry.held;
    entry.held = null;
    if (moved) boxChanged(pg, b); // re-previews, superseding the held reply
    else if (held?.seq === entry.previewSeq) applyPreview(entry, held.seq, held.r, held.err);
  };
  node.addEventListener('pointermove', move);
  node.addEventListener('pointerup', end);
  node.addEventListener('pointercancel', end);
  node.addEventListener('lostpointercapture', end);
}

function keyBox(e, pg, b, node) {
  if (e.target !== node || !live(pg)) return; // keys on the × are the button's
  if (e.key === 'Delete' || e.key === 'Backspace') {
    e.preventDefault();
    if (!(pg.skipped && !pg.edited)) removeBox(pg, b);
    return;
  }
  const step = e.shiftKey ? NUDGE_BIG_IN : NUDGE_IN;
  const dx = { ArrowLeft: -step, ArrowRight: step }[e.key] || 0;
  const dy = { ArrowUp: -step, ArrowDown: step }[e.key] || 0;
  if (!dx && !dy) return;
  e.preventDefault();
  const was = [b.x, b.y, b.w, b.h];
  if (e.altKey) {
    b.w = clamp(b.w + dx / pg.widthIn, Math.min(b.w, MIN_IN / pg.widthIn), 1 - b.x);
    b.h = clamp(b.h + dy / pg.heightIn, Math.min(b.h, MIN_IN / pg.heightIn), 1 - b.y);
  } else {
    b.x = clamp(b.x + dx / pg.widthIn, 0, 1 - b.w);
    b.y = clamp(b.y + dy / pg.heightIn, 0, 1 - b.h);
  }
  if ([b.x, b.y, b.w, b.h].some((v, k) => v !== was[k])) boxChanged(pg, b);
}

// A detected doc tab: dashed while left off, solid once it prints. Printed
// separately it is a label of its own, so it gets that label's number and an
// × that leaves it off again. A tab whose box was moved (oldTab) stays dashed.
function tabOutline(pg, b, i, num) {
  const node = el('div', { className: 'crop-tab' + (b.choice === 'off' ? '' : ' on'), title: 'doc tab' });
  place(node, b.tab || b.oldTab);
  if (separate(b)) {
    // a tab is often a narrow strip: stack the number and × along it
    if (b.tab.h * pg.heightIn > b.tab.w * pg.widthIn) node.classList.add('tall');
    node.append(el('span', { className: 'crop-num', textContent: num + 1 }));
    const del = el('button', { type: 'button', className: 'crop-del', textContent: '×' });
    del.setAttribute('aria-label', `stop printing the doc tab of label ${num} on its own (label ${num + 1}, ${pageRef(pg)})`);
    del.addEventListener('click', () => { setChoice(pg, b, 'off'); focusBox(pg, i); });
    node.append(del);
  }
  return node;
}

// What shrinking a label to fit does to its barcodes. Carriers ask for true
// size; a 15 mil module at 87% is 13 mil, the USPS floor.
const SCALE_WARN = 0.95;
const SCALE_BAD = 0.87;
function scaleNote(scale, advice = '; check the label box or the label size') {
  if (!(scale < SCALE_WARN)) return '';
  const pct = Math.round(scale * 100);
  return scale < SCALE_BAD
    ? ` · scaled to ${pct}% — barcodes likely won't scan${advice}`
    : ` · scaled to ${pct}%, barcodes may not scan`;
}

// What "include" would scale the label and its tab to, together: rendered to
// fit the label size, never enlarged. The preview sends the server's own
// figure (tabScale); this is for a label past the preview's cap. It fits the
// boxes, padding and all, where the server fits their ink, so it comes out a
// little low: shown as "about", it errs toward the stronger warning.
function includeScale(pg, b) {
  const m = state.pdf.media;
  if (!m) return 1;
  const u = unionBox(b, b.tab);
  let w = u.w * pg.widthIn, h = u.h * pg.heightIn;
  if ((w > h) !== (m.w > m.h)) [w, h] = [h, w];
  return Math.min(1, m.w / w, m.h / h);
}

// The segmented leave off / include / print separately control under a label
// with a doc tab (li is that label's image, while the preview is current).
// Buttons carry data-tool so focus survives the redraw a choice makes. "Print
// separately" can't take the page past MAX_BOXES; it stays focusable
// (aria-disabled) so the reason beside it is read out.
function tabChoice(pg, b, num, li) {
  const id = `tab-${pg.page}-${b.autoIndex}`;
  const seg = el('div', { className: 'seg seg-sm', role: 'group' });
  seg.setAttribute('aria-label', `doc tab of label ${num}`);
  // the server's figure: once included and rendered, the label's own
  const exact = b.choice === 'include' && li ? li.scale : b.tabScale;
  const scale = exact || includeScale(pg, b);
  const pct = `${exact ? '' : 'about '}${Math.round(scale * 100)}%`;
  const full = !separate(b) && flatBoxes(pg).length + 1 > MAX_BOXES;
  const hints = [];
  const hint = (key, text, warn) => {
    const p = el('p', { className: 'hint' + (warn ? ' warn' : ''), id: `${id}-${key}`, textContent: text });
    hints.push(p);
    return p.id;
  };
  for (const [v, text] of [['off', 'leave off'], ['include', state.pdf.media ? `include · scaled to ${pct}` : 'include'], ['separate', 'print separately']]) {
    const btn = el('button', { type: 'button', textContent: text });
    btn.dataset.tool = `${id}-${v}`;
    btn.setAttribute('aria-pressed', String(b.choice === v));
    // the label's own caption says it once included, from the server's render
    if (v === 'include' && b.choice !== 'include' && scale < SCALE_WARN) {
      btn.setAttribute('aria-describedby', hint('scale', `include${scaleNote(scale, TAB_ADVICE)}`, true));
    }
    if (v === 'separate' && full) {
      btn.setAttribute('aria-disabled', 'true');
      btn.setAttribute('aria-describedby', hint('full', `print separately: ${isImage() ? 'an image' : 'a page'} holds at most ${MAX_BOXES} labels`));
    }
    btn.addEventListener('click', () => { if (btn.getAttribute('aria-disabled') !== 'true') setChoice(pg, b, v); });
    seg.append(btn);
  }
  return el('div', { className: 'tab-choice' }, el('span', { className: 'caption', textContent: 'doc tab' }), seg, ...hints);
}

const TAB_ADVICE = '; leave the doc tab off or print it separately';

// The flip under a label turns it 180°, for a sender who put the label on the
// page upside down. num is the label's print number. The flip belongs to the
// box, so a doc tab printed separately turns with its label, whichever toggle
// is pressed. It keeps the doc-tab choice, so it calls markEdited, not
// boxChanged, and it stays enabled while the page updates: the last press
// wins. data-tool puts focus back on it after the redraw.
function flipToggle(pg, b, num) {
  const btn = el('button', { type: 'button', textContent: 'flip' });
  btn.dataset.tool = `flip-${num}`;
  btn.setAttribute('aria-pressed', String(!!b.flip));
  btn.setAttribute('aria-label', `flip label ${num} of ${pageRef(pg)}`);
  btn.addEventListener('click', () => {
    if (!live(pg)) return;
    b.flip = !b.flip;
    markEdited(pg);
  });
  return el('div', { className: 'seg seg-sm' }, btn);
}

function labelNote(li, advice) {
  if (!li) return 'not previewed';
  if (li.method === 'native') return `original ${li.ppi} dpi bitmap, unscaled`;
  const m = state.pdf.media;
  return (m ? `rendered for ${fmtIn(m.w)} × ${fmtIn(m.h)} in` : 'rendered') + scaleNote(li.scale, advice);
}

// The labels a page makes, in print order. Under each: its flip, then, for a
// box with a doc tab, the tab choice, then the barcode check. A tab printed
// separately follows as a label of its own.
function labelFigures(pg) {
  const total = flatBoxes(pg).length;
  if (!total) return []; // nothing printing (a skipped page, or every box removed)
  const figs = [];
  let k = 0;
  for (const b of pg.boxes) {
    const num = k + 1;
    const included = !!b.tab && b.choice === 'include';
    const suffixes = separate(b) ? ['', ' · doc tab'] : [included ? ' · with doc tab' : ''];
    suffixes.forEach((suffix, j) => {
      const li = pg.stale ? null : pg.labelImages[k];
      const old = pg.stale && pg.labelImages[k];
      const name = `label ${k + 1}${total > 1 ? `/${total}` : ''}${suffix}`;
      const fig = el('figure', { className: 'preview-item label-item' + (old ? ' stale' : '') });
      if (li || old) fig.append(el('img', { src: (li || old).image, alt: name, loading: 'lazy' }));
      const cap = el('figcaption', { textContent: `${name} · ${pg.stale ? 'updating…' : labelNote(li, included ? TAB_ADVICE : undefined)}` });
      if (li && li.scale < SCALE_WARN) cap.classList.add('warn');
      fig.append(cap, flipToggle(pg, b, k + 1));
      if (j === 0 && b.tab) fig.append(tabChoice(pg, b, num, li));
      if (li) fig.append(checkLine(pg.page, k)); // hidden while stale: there's no li
      figs.push(fig);
      k++;
    });
  }
  return figs;
}

// ---------------------------------------------------------------- barcode check
//
// A pdf or image preview comes with a check token. While that preview is on
// screen the page long-polls /api/check/{token}, and each label gets a line
// saying whether its barcode reads, and its tracking number. A newer
// preview, another file or a failed preview stops it: the open poll is
// aborted, so the server stops counting the token as polled and cancels its
// work. A tracking number stays on the page: not in the title, storage or
// the job name.

const CHECK_GIVE_UP_MS = 30000;

// stopCheck on a closed entry touches only that entry's poll, never the open
// one's.
function stopCheck(e) {
  const had = !!e.check;
  e.checkAbort?.abort();
  e.checkAbort = null;
  e.check = null;
  if (had && e === state) showChecks();
  release(e, 'check');
}

// The open entry's check is polled at once; a closed entry's waits for the
// scheduler.
function startCheck(e, token) {
  stopCheck(e);
  if (!token) return; // nothing to check, or too big to hold: no lines
  e.check = { token, labels: new Map(), failed: false, done: false };
  if (e === state) pollCheck(e);
}

// pollCheck polls e's check under a new AbortController (e.checkAbort).
// Re-polls straight away until done; gives up after CHECK_GIVE_UP_MS. A lost
// connection is "didn't finish", never an error banner. A 404 (the token
// expired, was evicted or was cancelled) gets one re-preview for a fresh
// token (retryCheck); a second is "didn't finish".
async function pollCheck(e) {
  const chk = e.check;
  const ctrl = new AbortController();
  e.checkAbort = ctrl;
  const signal = ctrl.signal;
  const started = Date.now();
  for (;;) {
    let r;
    try {
      r = await api(`/api/check/${encodeURIComponent(chk.token)}`, { signal });
    } catch (err) {
      if (signal.aborted) return;
      if (err.status === 404 && !e.checkRetried && e.check === chk) {
        retryCheck(e, ctrl);
        return;
      }
      chk.failed = true;
      break;
    }
    if (signal.aborted || e.check !== chk) return;
    if (!r) { // a 200 that isn't json: an auth proxy's login page, say
      chk.failed = true;
      break;
    }
    for (const l of r.labels || []) chk.labels.set(`${l.page}:${l.index}`, l);
    if (r.done) break;
    if (Date.now() - started > CHECK_GIVE_UP_MS) {
      chk.failed = true;
      break;
    }
    if (e === state) showChecks(); // the labels finished so far
  }
  chk.done = true;
  if (e.checkAbort === ctrl) e.checkAbort = null;
  if (e.check === chk && e === state) showChecks();
  renderQueue();
  release(e, 'check');
}

// retryCheck re-previews e for a fresh check token, once (checkRetried is
// reset by the user's own edits and by a label-size change): the open entry
// at once, a closed one through the scheduler.
function retryCheck(e, ctrl) {
  e.checkRetried = true;
  if (e.checkAbort === ctrl) e.checkAbort = null;
  if (app.bg?.e === e) app.bg = null;
  e.pending = true;
  if (e === state) requestPreview(e);
  else setTimeout(kick);
}

// A label's result: pending until it has one, "timeout" once the check is
// given up; null when the entry's preview has no check.
function checkResult(e, page, index) {
  const chk = e.check;
  if (!chk) return null;
  const l = chk.labels.get(`${page}:${index}`);
  if (l && l.status !== 'pending') return l;
  return { status: chk.failed ? 'timeout' : 'pending' };
}

// the line under label image index of page, kept up to date by showChecks
function checkLine(page, index) {
  const p = el('p', { className: 'check-line' });
  p.dataset.page = page;
  p.dataset.index = index;
  fillCheckLine(p);
  return p;
}

function fillCheckLine(p) {
  const l = checkResult(state, p.dataset.page, p.dataset.index);
  p.className = 'check-line';
  p.hidden = !l;
  if (!l) {
    p.replaceChildren();
    return;
  }
  switch (l.status) {
    case 'pending':
      p.textContent = 'checking barcode…';
      break;
    case 'ok':
      p.classList.add('ok');
      p.replaceChildren(`${l.carrier} · `, el('span', { className: 'tracking', textContent: l.tracking }), ' · barcode reads ✓');
      break;
    case 'reads':
      p.textContent = 'barcode reads, no tracking number recognised';
      break;
    case 'none':
      p.classList.add('warn');
      p.textContent = '⚠ no barcode could be read — check this label before printing';
      break;
    default: // timeout, error
      p.textContent = "barcode check didn't finish";
  }
}

// Fills in the lines in place: redrawing the pages would end a drag.
function showChecks() {
  for (const p of $('preview-grid').querySelectorAll('.check-line')) fillCheckLine(p);
  updateSummary();
}

// Checked labels on pages that aren't waiting for a re-preview, whose
// barcode couldn't be read. Labels past the preview's cap aren't checked.
function unreadLabels(e) {
  const chk = e.check;
  if (!chk || !paged(e.kind)) return 0;
  let n = 0;
  for (const pg of e.pdf.pages.values()) {
    if (pg.stale) continue;
    for (let k = 0; k < pg.labelImages.length; k++) {
      if (chk.labels.get(`${pg.page}:${k}`)?.status === 'none') n++;
    }
  }
  return n;
}

// ZPL's tracking numbers come from its ^BC/^B3 field data, not a decode, so
// they carry no ✓.
function zplTracking(entries) {
  if (!entries?.length) return;
  const box = el('div', { className: 'zpl-tracking' }, ...entries.map((e) => el('p', { className: 'caption' },
    `tracking · ${e.carrier} · `, el('span', { className: 'tracking', textContent: e.value }), ' (from the zpl)')));
  $('preview-grid').after(box);
}

// the zpl itself, collapsed unless there's no picture to look at
function zplSource(e, open) {
  const pre = el('pre', { className: 'preview-zpl', tabIndex: 0 });
  const details = el('details', { className: 'zpl-source', open }, el('summary', { textContent: 'zpl source' }), pre);
  $('preview-grid').after(details);
  e.file.slice(0, 4096).text().then((t) => {
    pre.textContent = t + (e.file.size > 4096 ? '\n…' : '');
  });
}

function updateSummary() {
  renderQueue();
  const btn = $('print');
  const copies = wholeCopies(state.copies);
  const copiesOK = copies >= 1 && copies <= app.maxCopies;
  if (copiesOK) $('copies').removeAttribute('aria-invalid');
  else $('copies').setAttribute('aria-invalid', 'true');
  const empty = paged(state.kind) ? emptyPages(state) : [];
  btn.disabled = !state.kind || !copiesOK || empty.length > 0 || !!state.previewError;
  const sum = $('summary');
  if (!state.file) { sum.textContent = 'choose a file to start'; return; }
  if (!state.kind) { sum.textContent = ''; return; }
  if (state.previewError) { sum.textContent = state.previewError; return; }
  if (!copiesOK) { sum.textContent = `copies: a whole number, 1 to ${app.maxCopies}`; return; }
  let units = 1;
  if (paged(state.kind)) units = pdfLabelCount(state);
  const total = units * copies;
  if (empty.length) {
    sum.textContent = isImage()
      ? 'image has no labels — add one or choose whole image'
      : `page ${empty[0].page} has no labels — add one, reset it, or leave it out of the page range`;
    return;
  }
  const unread = unreadLabels(state);
  sum.textContent = (copies > 1 && units > 1
    ? `${units} labels × ${copies} copies = ${total} labels`
    : `${total} label${total === 1 ? '' : 's'}`) +
    (unread ? ` · ${unread} label${unread === 1 ? '' : 's'} whose barcode couldn't be read` : '');
}
// #copies and #pages mirror the open entry: their listeners write into it.
$('copies').addEventListener('input', () => {
  state.copies = $('copies').value;
  updateSummary();
});

// Scheduling a preview makes any reply still on its way out of date: it was
// asked for before this edit, and showing it would draw the old boxes' labels
// as current (and clear "updating…") until the new reply lands. The debounce
// is the entry's own, so it fires for that entry whichever file is open by
// then.
function schedulePreview(e) {
  if (!e.file) return;
  e.pending = true;
  e.checkRetried = false;
  e.previewSeq++;
  stopCheck(e);
  clearTimeout(e.timer);
  e.timer = setTimeout(() => {
    e.timer = null;
    requestPreview(e);
  }, 450);
}
// the boxes on screen stop being editable at once (see live): dim them until the reply
$('pages').addEventListener('input', () => {
  state.pages = $('pages').value;
  clearEdits(state);
  if (state.kind === 'pdf') $('preview').classList.add('loading');
  schedulePreview(state);
});
// An image's ppi can be matched to the label size, so it re-previews too.
$('media').addEventListener('change', () => {
  if (paged(state.kind)) clearEdits(state);
  if (state.kind === 'zpl' || paged(state.kind)) requestPreview(state);
});
for (const radio of document.querySelectorAll('input[name="layout"]')) {
  radio.addEventListener('change', () => {
    state.layout = radio.value;
    state.checkRetried = false; // the user's own edit: a later 404 gets its own retry
    clearEdits(state);
    requestPreview(state);
  });
}

// ---------------------------------------------------------------- drag and drop

const drop = $('drop');
let dragDepth = 0;
window.addEventListener('dragover', (e) => e.preventDefault()); // stop the browser opening a missed drop
window.addEventListener('drop', (e) => e.preventDefault());
drop.addEventListener('dragenter', (e) => { e.preventDefault(); dragDepth++; drop.classList.add('dragging'); });
drop.addEventListener('dragleave', () => { if (--dragDepth <= 0) { dragDepth = 0; drop.classList.remove('dragging'); } });
drop.addEventListener('drop', (e) => {
  e.preventDefault();
  dragDepth = 0;
  drop.classList.remove('dragging');
  addFiles(e.dataTransfer?.files || []);
});
// value is cleared, so choosing the same file again adds it again
$('file').addEventListener('change', (e) => {
  const files = [...e.target.files];
  e.target.value = '';
  addFiles(files);
});
$('clear').addEventListener('click', () => removeEntry(state));

// ---------------------------------------------------------------- print

$('form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const entry = state;
  if (!entry.kind) return;
  const btn = $('print');
  btn.disabled = true;
  btn.textContent = 'printing…';
  showResult('info', paged(entry.kind) ? 'rendering and sending to lprint…' : 'sending to lprint…');
  const fields = { ...requestFields(entry), copies: entry.copies };
  if (entry.kind === 'zpl') delete fields.media;
  try {
    const r = await api('/api/print', { method: 'POST', body: formData(entry, fields) });
    r.jobs.forEach((j) => app.mine.add(j.id));
    app.lastResult = r;
    renderPrintResult();
    resetCopies(fields.copies);
  } catch (err) {
    const sent = err.body?.jobs || [];
    sent.forEach((j) => app.mine.add(j.id));
    showResult('err', err.message);
  } finally {
    btn.textContent = 'print';
    updateSummary();
    loadJobs();
    loadPrinter();
  }
});

// Re-rendered as job states come in from Get-Jobs: lprint accepts a job
// before the printer has done anything, so "sent" isn't "printed".
function renderPrintResult(liveJobs) {
  const r = app.lastResult;
  if (!r) return;
  const byId = new Map((liveJobs || []).map((j) => [j.id, j]));
  const jobs = r.jobs.map((j) => byId.get(j.id) || j);
  const failed = jobs.filter((j) => j.state === 'aborted' || j.state === 'canceled');
  const done = jobs.filter((j) => j.state === 'completed').length;
  const ids = jobs.map((j) => j.id);
  const idText = ids.length === 1 ? `job ${ids[0]}` : `jobs ${ids[0]}–${ids[ids.length - 1]}`;

  const lines = [];
  let tone;
  if (failed.length) {
    tone = 'err';
    lines.push(`${idText}: ${failed.length} ${failed[0].state}`);
    for (const j of failed) lines.push(`job ${j.id}: ${j.message || j.reasons?.join(', ') || j.state}`);
  } else if (done === jobs.length) {
    tone = 'ok';
    lines.push(`printed · ${idText}`);
  } else {
    tone = r.warnings?.length ? 'warn' : 'info';
    lines.push(`sent to lprint · ${idText} · ${done ? `${done} of ${jobs.length} done` : jobs[0].state}`);
  }
  showResult(tone, lines[0], [...lines.slice(1), ...(r.warnings || [])]);
}

function showResult(tone, headline, details = []) {
  const box = $('result');
  box.className = 'result ' + tone;
  box.replaceChildren(el('strong', { textContent: headline }));
  if (details.length) box.append(el('ul', {}, ...details.map((d) => el('li', { textContent: d }))));
  box.hidden = false;
}

// ---------------------------------------------------------------- jobs

const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto', style: 'short' });
function ago(iso) {
  if (!iso) return '';
  const s = (Date.parse(iso) - Date.now()) / 1000;
  const abs = Math.abs(s);
  if (abs < 45) return 'just now';
  if (abs < 3600) return rtf.format(Math.round(s / 60), 'minute');
  if (abs < 86400) return rtf.format(Math.round(s / 3600), 'hour');
  return rtf.format(Math.round(s / 86400), 'day');
}

const stateBadge = {
  completed: 'badge-success', processing: 'badge-accent', pending: 'badge-accent',
  held: 'badge-warning', stopped: 'badge-warning', aborted: 'badge-danger', canceled: 'badge-danger',
};

let jobsTimer;
async function loadJobs() {
  clearTimeout(jobsTimer);
  let jobs = [];
  let busy = false;
  try {
    ({ jobs } = await api('/api/jobs'));
    renderJobs(jobs);
    $('jobs-updated').textContent = 'updated ' + new Date().toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
    busy = jobs.some((j) => !terminal.has(j.state));
    if (app.lastResult) renderPrintResult(jobs);
  } catch (e) {
    lastJobs = null;
    $('jobs-body').replaceChildren(el('tr', {}, el('td', { colSpan: 4, className: 'muted', textContent: e.message })));
  }
  // poll quickly while something is in flight, lazily otherwise
  jobsTimer = setTimeout(loadJobs, busy ? 2000 : 20000);
}

function renderJobs(jobs) {
  lastJobs = jobs;
  pruneCancels(jobs);
  const body = $('jobs-body');
  if (!jobs.length) {
    body.replaceChildren(el('tr', {}, el('td', { colSpan: 4, className: 'muted', textContent: 'no jobs yet' })));
    return;
  }
  // the table is rebuilt on every poll; keep keyboard focus on a cancel button
  const focused = document.activeElement?.dataset?.cancel;
  body.replaceChildren(...jobs.map((j) => {
    const name = el('td', { className: 'name', textContent: j.name || 'untitled' });
    const sub = [j.user, j.state !== 'completed' ? j.message : ''].filter(Boolean).join(' · ');
    if (sub) name.append(el('span', { className: 'sub', textContent: sub }));
    const when = el('td', { className: 'when', textContent: ago(j.completed || j.created) });
    if (j.created) when.title = new Date(j.created).toLocaleString();
    return el('tr', { className: app.mine.has(j.id) ? 'mine' : '' },
      el('td', { className: 'num', textContent: j.id }),
      name,
      el('td', {}, el('div', { className: 'job-state' },
        el('span', { className: 'badge ' + (stateBadge[j.state] || 'badge-neutral'), textContent: j.state }),
        ...cancelControls(j))),
      when);
  }));
  if (focused) body.querySelector(`[data-cancel="${focused}"]`)?.focus();
}

// ---------------------------------------------------------------- cancel

// A held or pending job prints the moment the printer comes back, so any job
// still in lprint's queue can be canceled. Canceling can't be undone, so it
// takes two clicks: "cancel", then "confirm?" within a few seconds. Progress
// and lprint's answer live here, outside the rows, because every poll
// rebuilds the table.
const cancelable = new Set(['pending', 'held', 'processing', 'stopped']);
const cancels = new Map(); // job id → { step: armed | sending | sent | error, msg, timer }
let lastJobs = null;       // the list on screen, to redraw without a fetch

function setCancel(id, entry, ttl) {
  clearTimeout(cancels.get(id)?.timer);
  if (entry) {
    cancels.set(id, entry);
    if (ttl) entry.timer = setTimeout(() => { if (cancels.get(id) === entry) setCancel(id, null); }, ttl);
  } else {
    cancels.delete(id);
  }
  if (lastJobs) renderJobs(lastJobs);
}

// drop what no longer applies: jobs that left the list, and finished jobs
// (an error stays until it times out, so "already completed" can be read)
function pruneCancels(jobs) {
  const byId = new Map(jobs.map((j) => [j.id, j]));
  for (const [id, c] of cancels) {
    const j = byId.get(id);
    if (!j || (terminal.has(j.state) && c.step !== 'error')) {
      clearTimeout(c.timer);
      cancels.delete(id);
    }
  }
}

function cancelControls(j) {
  const c = cancels.get(j.id);
  const out = [];
  if (cancelable.has(j.state)) {
    if (c?.step === 'sent') {
      // a processing job stops at the next page, so it can linger a moment
      out.push(el('span', { className: 'sub', textContent: 'canceling…' }));
    } else {
      const armed = c?.step === 'armed';
      const sending = c?.step === 'sending';
      const btn = el('button', {
        type: 'button',
        className: 'btn btn-ghost btn-sm job-cancel' + (armed ? ' armed' : ''),
        textContent: sending ? 'canceling…' : armed ? 'confirm?' : 'cancel',
      });
      btn.dataset.cancel = j.id;
      btn.setAttribute('aria-label', armed ? `confirm canceling job ${j.id}` : `cancel job ${j.id}`);
      // aria-disabled rather than disabled, so keyboard focus survives the redraw
      if (sending) btn.setAttribute('aria-disabled', 'true');
      btn.addEventListener('click', () => cancelClick(j.id));
      out.push(btn);
    }
  }
  if (c?.step === 'error') out.push(el('span', { className: 'sub cancel-error', textContent: c.msg }));
  return out;
}

async function cancelClick(id) {
  const step = cancels.get(id)?.step;
  if (step === 'sending' || step === 'sent') return;
  if (step !== 'armed') {
    setCancel(id, { step: 'armed' }, 4000);
    return;
  }
  setCancel(id, { step: 'sending' });
  try {
    await api(`/api/jobs/${id}/cancel`, { method: 'POST' });
    setCancel(id, { step: 'sent' });
  } catch (e) {
    setCancel(id, { step: 'error', msg: e.message }, 10000);
  }
  loadJobs();
}

// ---------------------------------------------------------------- paste

// A file on the clipboard (a copied pdf, a screenshot) is added to the queue
// and opened; only the first, and of any type. In a text field the field gets
// the paste if there's text for it, so pasting into pages or copies works as
// usual; a file with no text (a screenshot) still loads, since the field
// couldn't take it anyway.
document.addEventListener('paste', (e) => {
  const f = e.clipboardData?.files?.[0];
  if (!f) return;
  const field = e.target.closest?.('input:not([type=file]), textarea, [contenteditable]');
  if (field && e.clipboardData.types.includes('text/plain')) return;
  e.preventDefault();
  addFiles([f], { open: true });
});

// ---------------------------------------------------------------- remembered settings

// Per browser, like the theme. Only the size and the copies count are kept,
// nothing about the file. The size applies only while lprint still lists it
// (fillMedia checks); copies go back to 1 once a print succeeds, so a ×3
// can't carry over to the next label by accident.
const PREF_MEDIA = 'labelprint-media';
const PREF_COPIES = 'labelprint-copies';
const pref = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* storage blocked */ } },
  del(k) { try { localStorage.removeItem(k); } catch { /* storage blocked */ } },
};

// The size picked in this tab. fillMedia falls back to the default while
// lprint doesn't list it, and puts it back when lprint lists it again; until
// something is picked here, the saved size plays that part (so it still lands
// if lprint was unreachable when the page loaded).
let mediaChoice = null;
$('media').addEventListener('change', () => {
  mediaChoice = $('media').value;
  pref.set(PREF_MEDIA, mediaChoice);
});

// whole numbers only: the server refuses anything else, so "2.5" isn't saved as 2
const wholeCopies = (v) => (/^\d+$/.test(v.trim()) ? parseInt(v, 10) : NaN);

// Any entry's copies, as typed, become the default new entries start from.
$('copies').addEventListener('input', () => {
  const n = wholeCopies($('copies').value);
  if (n >= 1 && n <= app.maxCopies) app.defaultCopies = String(n);
  if (n === 1) pref.del(PREF_COPIES);
  else if (n > 1 && n <= app.maxCopies) pref.set(PREF_COPIES, String(n));
});

// After a successful print, unless the count was changed while it was sending.
function resetCopies(sent) {
  if ($('copies').value !== sent) return;
  app.defaultCopies = '1';
  state.copies = '1';
  $('copies').value = '1';
  pref.del(PREF_COPIES);
}

// Called once the real copies limit (MAX_COPIES) has come from /api/printer,
// before any file is open.
function restoreCopies() {
  const n = wholeCopies(pref.get(PREF_COPIES) || '');
  if (!(n > 1 && n <= app.maxCopies)) return;
  app.defaultCopies = String(n);
  state.copies = app.defaultCopies;
  $('copies').value = state.copies;
}

// ---------------------------------------------------------------- feed a label, test label

// One ordinary zpl job through lprint, so it lands in the jobs table like a
// print. The server holds the zpl; the page only names the action (and, for
// the test label, the size to lay it out for). The result shows as a badge in
// the printer status, which is already a live region.
let feedTimer;
function feedStatus(tone, text) {
  clearTimeout(feedTimer);
  const s = $('feed-status');
  s.className = `badge badge-${tone} feed-status`;
  s.textContent = s.title = text;
  s.hidden = !text;
}

const printerActions = [
  { id: 'feed', action: 'feed', busy: 'feeding…', what: 'feed' },
  { id: 'test-label', action: 'test-label', busy: 'printing…', what: 'test label', sized: true },
];
for (const a of printerActions) {
  const btn = $(a.id);
  const idle = btn.textContent;
  btn.addEventListener('click', async () => {
    // one at a time: both buttons wait for this job
    for (const o of printerActions) $(o.id).disabled = true;
    btn.textContent = a.busy;
    feedStatus('neutral', '');
    try {
      const body = { action: a.action };
      if (a.sized && $('media').value) body.media = $('media').value;
      const r = await api('/api/printer/maintenance', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      r.jobs.forEach((j) => app.mine.add(j.id));
      const warn = r.warnings?.length ? ` · ${r.warnings.join(', ')}` : '';
      feedStatus('success', `${a.what} sent · job ${r.jobs[0].id}${warn}`);
      feedTimer = setTimeout(() => feedStatus('neutral', ''), 10000);
    } catch (e) {
      feedStatus('danger', `${a.what} failed: ${e.message}`);
    } finally {
      for (const o of printerActions) $(o.id).disabled = false;
      btn.textContent = idle;
      loadJobs();
      loadPrinter();
    }
  });
}

// ---------------------------------------------------------------- theme

$('theme').addEventListener('click', () => {
  const next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  try { localStorage.setItem('owine-theme', next); } catch { /* storage blocked */ }
});

// ---------------------------------------------------------------- shared from a phone

// A file shared from a phone arrives as /?share=<token> (see share.go): fetch
// it once and treat it like a chosen file, so it lands on the preview. The
// query goes at once, so a reload can't ask for a token that's already used.
const shareErrors = {
  'too-big': () => `that file is over the ${app.maxMB} MB limit`,
  unsupported: () => "labelprint can't print that file — share a zpl, pdf, png, jpeg or gif",
  epl2: () => "EPL2 isn't supported — choose zpl or pdf in your shipping tool",
  unreadable: () => "couldn't read the shared file — try sharing it again",
};

async function fetchShared() {
  const q = new URLSearchParams(location.search);
  const token = q.get('share');
  const failed = q.get('share-error');
  if (token === null && failed === null) return null;
  history.replaceState(null, '', location.pathname + location.hash);
  if (failed !== null) return { error: (shareErrors[failed] || shareErrors.unreadable)() };
  let res;
  try {
    res = await fetch('/api/share/' + encodeURIComponent(token), { cache: 'no-store' });
  } catch {
    return { error: "can't reach labelprint to collect the shared file — share it again" };
  }
  if (!res.ok) {
    return { error: res.status === 404 ? 'that shared file has expired — share it again' : shareErrors.unreadable() };
  }
  const blob = await res.blob();
  return { file: new File([blob], dispositionName(res.headers.get('Content-Disposition')), { type: blob.type }) };
}

// filename*=utf-8''… (non-ascii names) or filename=…, as Go's mime.FormatMediaType writes them
function dispositionName(cd) {
  const ext = /filename\*=utf-8''([^;]+)/i.exec(cd || '');
  if (ext) { try { return decodeURIComponent(ext[1]); } catch { /* fall through */ } }
  const plain = /filename=(?:"((?:\\.|[^"\\])*)"|([^;\s]+))/i.exec(cd || '');
  if (plain) return plain[1] !== undefined ? plain[1].replace(/\\(.)/g, '$1') : plain[2];
  return 'shared file';
}

// No fetch handler: see sw.js for why it exists at all.
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});

// ---------------------------------------------------------------- boot

(async () => {
  // in parallel with the printer check
  const shared = fetchShared().catch(() => ({ error: shareErrors.unreadable() }));
  const p = await loadPrinter();
  if (p) {
    app.maxMB = p.maxUploadMB;
    app.maxCopies = p.maxCopies;
    $('max-mb').textContent = p.maxUploadMB;
    $('copies').max = p.maxCopies;
  }
  restoreCopies();
  updateSummary();
  fillMedia(p || { media: [{ name: 'na_index-4x6_4x6in', label: '4 × 6 in' }], defaultMedia: 'na_index-4x6_4x6in' });
  // after the size limit and media list are known, which the preview needs
  const s = await shared;
  if (s?.file) addFiles([s.file]);
  else if (s?.error) say(s.error);
  loadJobs();
  setInterval(async () => { const q = await loadPrinter(); if (q) fillMedia(q); }, 30000);
})();

// labelprint front end. No framework, no build step: everything the server
// sends that came from a file or from lprint goes into the DOM as text.
'use strict';

const $ = (id) => document.getElementById(id);
const el = (tag, props = {}, ...children) => {
  const node = Object.assign(document.createElement(tag), props);
  for (const c of children) node.append(c);
  return node;
};

const state = {
  file: null,
  kind: null,          // server-confirmed: zpl | pdf | png | jpeg
  objectURL: null,
  previewSeq: 0,       // drops out-of-order preview replies
  mine: new Set(),     // job ids submitted from this page
  lastResult: null,    // { jobs, warnings } of the last print, for live updates
  maxMB: 20,
  maxCopies: 100,
  layout: 'auto',      // label area for pdfs: auto | page
  pdf: { pages: new Map(), media: null, labelsElsewhere: 0, meta: null },
  previewError: null,  // a field the last preview refused (pages, crops, layout): print waits for a good one
};

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
    throw err;
  }
  return body;
}

function formData(extra = {}) {
  const fd = new FormData();
  for (const [k, v] of Object.entries(extra)) fd.append(k, v);
  fd.append('file', state.file, state.file.name); // file last: fields first keeps the server's reads small
  return fd;
}

// ---------------------------------------------------------------- pdf label areas
//
// For PDFs the server finds label boxes on each page. The page shows them over
// the page image; edits go back as `crops` for that page. Boxes are fractions
// of the page as displayed: x, y, w, h from the top-left.

const MIN_IN = 0.5;   // the server refuses smaller boxes
const MAX_BOXES = 8;  // per page, as the server allows
const NUDGE_IN = 0.05;     // arrow keys move or resize a box this far
const NUDGE_BIG_IN = 0.25; // with shift
const round4 = (v) => Math.round(v * 1e4) / 1e4;
const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
const fmtIn = (v) => String(Math.round(v * 100) / 100);
const arrBox = (a) => ({ x: a[0], y: a[1], w: a[2], h: a[3] });
const cloneBoxes = (bs) => bs.map((b) => ({ ...b, tab: b.tab ? { ...b.tab } : null }));

function clearEdits() {
  state.pdf.pages = new Map();
}

function resetPdf() {
  state.pdf = { pages: new Map(), media: null, labelsElsewhere: 0, meta: null };
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
function flatBoxes(pg) {
  if (pg.skipped && !pg.edited) return [];
  const out = [];
  for (const b of pg.boxes) {
    out.push(b.tab && b.choice === 'include' ? unionBox(b, b.tab) : b);
    if (separate(b)) out.push(growBox(b.tab, pg));
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
// grown to it before sending, since the server refuses smaller crops.
function cropsField() {
  const out = {};
  for (const pg of state.pdf.pages.values()) {
    if (!pg.edited || !pg.boxes.length) continue;
    out[pg.page] = flatBoxes(pg).map((b) => growBox(b, pg)).map((b) => [b.x, b.y, b.w, b.h].map(round4));
  }
  return Object.keys(out).length ? JSON.stringify(out) : '';
}

// requestFields are the form fields both preview and print send.
function requestFields() {
  const f = { pages: $('pages').value, media: $('media').value, layout: state.layout };
  const crops = cropsField();
  if (crops) f.crops = crops;
  return f;
}

// absorbPdf takes a preview reply, keeping the boxes of pages the user edited.
// An edited page also keeps what detection said about it (auto, fallback,
// skipped), since the reply describes the crops sent; reset goes back to that.
function absorbPdf(r) {
  state.pdf.media = r.widthIn ? { w: r.widthIn, h: r.heightIn } : null;
  state.pdf.meta = { selected: r.selected, pageCount: r.pageCount };
  const old = state.pdf.pages;
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
  state.pdf.labelsElsewhere = (r.labels || 0) - shownLabels;
  state.pdf.pages = next;
}

function pdfLabelCount() {
  let n = state.pdf.labelsElsewhere;
  for (const pg of state.pdf.pages.values()) n += flatBoxes(pg).length;
  return n;
}

// Pages the user left with no boxes. A skipped page isn't one: it prints
// nothing on purpose.
function emptyPages() {
  return [...state.pdf.pages.values()].filter((pg) => pg.edited && !pg.boxes.length);
}

// Whether pg is the page on screen. Changing the page range (or size, or
// layout) drops the pages before the reply that replaces them lands, so until
// then the boxes on screen belong to no page and edits to them are ignored.
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
  if (pg.boxes.length) schedulePreview();
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
  schedulePreview();
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

function chooseFile(file) {
  clearResult();
  if (state.objectURL) URL.revokeObjectURL(state.objectURL);
  state.objectURL = null;
  state.file = file;
  state.kind = null;
  state.previewError = null;
  resetPdf();
  state.layout = 'auto';
  document.querySelector('input[name="layout"][value="auto"]').checked = true;
  $('drop').classList.toggle('has-file', !!file);
  $('file-row').hidden = !file;
  $('pages').value = '';
  if (!file) {
    $('file').value = '';
    $('preview').hidden = true;
    $('pages-field').hidden = true;
    $('layout-field').hidden = true;
    setMediaHint(null);
    updateSummary();
    return;
  }
  $('file-name').textContent = file.name;
  $('file-size').textContent = fmtSize(file.size);
  $('file-kind').textContent = '…';
  if (file.size > state.maxMB * (1 << 20)) {
    showResult('err', `that file is ${fmtSize(file.size)} — the limit is ${state.maxMB} MB`);
    $('preview').hidden = true;
    updateSummary();
    return;
  }
  preview();
}

// For ZPL the size only shapes the preview (where the zpl doesn't set ^PW/^LL
// itself); the printer goes by the zpl, so it isn't sent with the job.
function setMediaHint(kind) {
  $('media-hint').textContent = kind === 'zpl' ? 'for the preview — the zpl sets the printed size' : '';
}

async function preview() {
  if (!state.file) return;
  const seq = ++state.previewSeq;
  const box = $('preview');
  box.classList.add('loading');
  $('pages').removeAttribute('aria-invalid');
  let r;
  try {
    r = await api('/api/preview', { method: 'POST', body: formData(requestFields()) });
  } catch (e) {
    if (seq !== state.previewSeq) return;
    box.classList.remove('loading');
    const field = e.body?.field;
    if (field) state.previewError = e.message;
    // The range, size or layout changed, so the pages on screen are gone from
    // state: take them away rather than leave boxes that edit nothing.
    if (field && state.kind === 'pdf' && !state.pdf.pages.size) {
      $('preview-grid').replaceChildren();
      $('preview-note').textContent = '';
    }
    if (field === 'pages') {
      // a bad page range: keep the file, flag the field
      $('pages').setAttribute('aria-invalid', 'true');
      $('pages-hint').textContent = e.message;
    } else if (field === 'crops' || field === 'layout') {
      const note = $('preview-note');
      note.textContent = e.message;
      note.classList.add('err');
    } else {
      state.kind = null;
      $('file-kind').textContent = 'rejected';
      $('file-kind').className = 'badge badge-danger';
      box.hidden = true;
      showResult('err', e.message);
    }
    updateSummary();
    return;
  }
  if (seq !== state.previewSeq) return;
  if (dragging) { heldPreview = { r, seq }; return; }
  showPreview(r);
}

function showPreview(r) {
  state.previewError = null;
  $('preview').classList.remove('loading');
  $('pages-hint').textContent = 'e.g. 1-3, 5';
  state.kind = r.kind;
  $('file-kind').textContent = r.kind;
  $('file-kind').className = 'badge badge-accent';
  $('pages-field').hidden = r.kind !== 'pdf';
  $('layout-field').hidden = r.kind !== 'pdf';
  setMediaHint(r.kind);
  renderPreview(r);
  updateSummary();
}

function renderPreview(r) {
  const grid = $('preview-grid');
  const note = $('preview-note');
  if (r.kind !== 'pdf') grid.replaceChildren(); // renderPdf swaps the pages itself, keeping focus
  grid.classList.toggle('pdf-mode', r.kind === 'pdf');
  note.classList.remove('err');
  document.querySelector('.zpl-source')?.remove();
  $('preview').hidden = false;
  if (r.kind === 'pdf') {
    absorbPdf(r);
    renderPdf();
  } else if (r.kind === 'png' || r.kind === 'jpeg') {
    state.objectURL = URL.createObjectURL(state.file);
    note.textContent = 'scaled to fit the label';
    grid.append(el('figure', { className: 'preview-item' },
      el('img', { src: state.objectURL, alt: state.file.name })));
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
    zplSource(!!r.previewError);
  }
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
  const n = pdfLabelCount();
  const m = state.pdf.media;
  note.textContent = `${selected} of ${pageCount} page${pageCount === 1 ? '' : 's'} · ${n} label${n === 1 ? '' : 's'}` +
    (m ? ` · black and white, sized for ${fmtIn(m.w)} × ${fmtIn(m.h)} in` : '') +
    (selected > shown ? ` · showing the first ${shown} pages` : '');
  grid.replaceChildren(...[...state.pdf.pages.values()].map(pageFigure));
  if (focus) focusTool({ page: focus.page }, focus.tool);
  if (focusCrop) focusBox({ page: focusCrop.page }, focusCrop.index, focusCrop.del);
}

function pageCaption(pg) {
  const n = pg.boxes.length;
  if (pg.edited) {
    const k = flatBoxes(pg).length;
    return `p${pg.page} · ${k} label area${k === 1 ? '' : 's'}, edited`;
  }
  if (pg.skipped) return `p${pg.page} · no label found, not printing`;
  if (pg.fallback) return `p${pg.page} · no label found, using the whole page`;
  if (pg.auto) return `p${pg.page} · ${n} label${n === 1 ? '' : 's'} found`;
  return `p${pg.page} · whole page`;
}

function pageFigure(pg) {
  const skipped = pg.skipped && !pg.edited;
  const stage = el('div', { className: 'page-stage' },
    el('img', { src: pg.image, alt: `page ${pg.page}`, draggable: false }));
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
    include.setAttribute('aria-label', `include page ${pg.page}: print it as a label`);
    include.dataset.tool = 'include';
    include.addEventListener('click', () => { if (!live(pg)) return; pg.edited = true; markEdited(pg); focusTool(pg, 'reset'); });
    tools.append(include);
  }
  const add = el('button', { type: 'button', className: 'btn btn-ghost btn-sm', textContent: 'add label' });
  add.setAttribute('aria-label', `add a label area to page ${pg.page}`);
  add.dataset.tool = 'add';
  add.disabled = flatBoxes(pg).length >= MAX_BOXES;
  if (add.disabled) add.title = `${MAX_BOXES} label areas is the most a page can have`;
  add.addEventListener('click', () => addBox(pg));
  tools.append(add);
  if (pg.edited) {
    const reset = el('button', { type: 'button', className: 'btn btn-ghost btn-sm', textContent: 'reset' });
    reset.setAttribute('aria-label', `reset page ${pg.page}`);
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
    ? `label area ${num} on page ${pg.page}, ${size}, not printing: move or resize it to include the page`
    : `label area ${num} on page ${pg.page}, ${size}${tab}: drag or arrow keys to move, ` +
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
    del.setAttribute('aria-label', `remove label area ${num} from page ${pg.page}`);
    del.addEventListener('pointerdown', (e) => e.stopPropagation());
    del.addEventListener('click', () => removeBox(pg, b));
    node.append(del);
  }
  node.addEventListener('pointerdown', (e) => startDrag(e, pg, b, node, e.target.dataset?.corner || null));
  node.addEventListener('keydown', (e) => keyBox(e, pg, b, node));
  return node;
}

// A preview reply that lands mid-drag is held until the drag ends: redrawing
// would swap the box being dragged for a new node and end the drag.
let dragging = false;
let heldPreview = null;

function startDrag(e, pg, b, node, corner) {
  if (e.button !== 0 || dragging || !live(pg)) return;
  e.preventDefault();
  node.focus();
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
    const held = heldPreview;
    heldPreview = null;
    if (moved) boxChanged(pg, b); // re-previews, superseding the held reply
    else if (held?.seq === state.previewSeq) showPreview(held.r);
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
    del.setAttribute('aria-label', `stop printing the doc tab of label ${num} on its own (label ${num + 1}, page ${pg.page})`);
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
      btn.setAttribute('aria-describedby', hint('full', `print separately: a page holds at most ${MAX_BOXES} labels`));
    }
    btn.addEventListener('click', () => { if (btn.getAttribute('aria-disabled') !== 'true') setChoice(pg, b, v); });
    seg.append(btn);
  }
  return el('div', { className: 'tab-choice' }, el('span', { className: 'caption', textContent: 'doc tab' }), seg, ...hints);
}

const TAB_ADVICE = '; leave the doc tab off or print it separately';

function labelNote(li, advice) {
  if (!li) return 'not previewed';
  if (li.method === 'native') return `original ${li.ppi} dpi bitmap, unscaled`;
  const m = state.pdf.media;
  return (m ? `rendered for ${fmtIn(m.w)} × ${fmtIn(m.h)} in` : 'rendered') + scaleNote(li.scale, advice);
}

// The labels a page makes, in print order. A box with a doc tab carries the
// tab choice under its label; a tab printed separately follows as its own.
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
      fig.append(cap);
      if (j === 0 && b.tab) fig.append(tabChoice(pg, b, num, li));
      figs.push(fig);
      k++;
    });
  }
  return figs;
}

// the zpl itself, collapsed unless there's no picture to look at
function zplSource(open) {
  const pre = el('pre', { className: 'preview-zpl', tabIndex: 0 });
  const details = el('details', { className: 'zpl-source', open }, el('summary', { textContent: 'zpl source' }), pre);
  $('preview-grid').after(details);
  state.file.slice(0, 4096).text().then((t) => {
    pre.textContent = t + (state.file.size > 4096 ? '\n…' : '');
  });
}

function updateSummary() {
  const btn = $('print');
  const copies = wholeCopies($('copies').value);
  const copiesOK = copies >= 1 && copies <= state.maxCopies;
  if (copiesOK) $('copies').removeAttribute('aria-invalid');
  else $('copies').setAttribute('aria-invalid', 'true');
  const empty = state.kind === 'pdf' ? emptyPages() : [];
  btn.disabled = !state.kind || !copiesOK || empty.length > 0 || !!state.previewError;
  const sum = $('summary');
  if (!state.file) { sum.textContent = 'choose a file to start'; return; }
  if (!state.kind) { sum.textContent = ''; return; }
  if (state.previewError) { sum.textContent = state.previewError; return; }
  if (!copiesOK) { sum.textContent = `copies: a whole number, 1 to ${state.maxCopies}`; return; }
  let units = 1;
  if (state.kind === 'pdf') units = pdfLabelCount();
  const total = units * copies;
  if (empty.length) {
    sum.textContent = `page ${empty[0].page} has no labels — add one, reset it, or leave it out of the page range`;
    return;
  }
  sum.textContent = copies > 1 && units > 1
    ? `${units} labels × ${copies} copies = ${total} labels`
    : `${total} label${total === 1 ? '' : 's'}`;
}
$('copies').addEventListener('input', () => updateSummary());

let previewTimer;
function schedulePreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(preview, 450);
}
// the boxes on screen stop being editable at once (see live): dim them until the reply
$('pages').addEventListener('input', () => {
  clearEdits();
  if (state.kind === 'pdf') $('preview').classList.add('loading');
  schedulePreview();
});
$('media').addEventListener('change', () => {
  if (state.kind === 'pdf') clearEdits();
  if (state.kind === 'zpl' || state.kind === 'pdf') preview();
});
for (const radio of document.querySelectorAll('input[name="layout"]')) {
  radio.addEventListener('change', () => { state.layout = radio.value; clearEdits(); preview(); });
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
  const f = e.dataTransfer?.files?.[0];
  if (f) chooseFile(f);
});
$('file').addEventListener('change', (e) => chooseFile(e.target.files[0] || null));
$('clear').addEventListener('click', () => chooseFile(null));

// ---------------------------------------------------------------- print

$('form').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!state.kind) return;
  const btn = $('print');
  btn.disabled = true;
  btn.textContent = 'printing…';
  showResult('info', state.kind === 'pdf' ? 'rendering and sending to lprint…' : 'sending to lprint…');
  const fields = { ...requestFields(), copies: $('copies').value };
  if (state.kind === 'zpl') delete fields.media;
  try {
    const r = await api('/api/print', { method: 'POST', body: formData(fields) });
    r.jobs.forEach((j) => state.mine.add(j.id));
    state.lastResult = r;
    renderPrintResult();
    resetCopies(fields.copies);
  } catch (err) {
    const sent = err.body?.jobs || [];
    sent.forEach((j) => state.mine.add(j.id));
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
  const r = state.lastResult;
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

function clearResult() {
  state.lastResult = null;
  $('result').hidden = true;
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
    if (state.lastResult) renderPrintResult(jobs);
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
    return el('tr', { className: state.mine.has(j.id) ? 'mine' : '' },
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

// A file on the clipboard (a copied pdf, a screenshot) loads like a drop. In a
// text field the field gets the paste if there's text for it, so pasting into
// pages or copies works as usual; a file with no text (a screenshot) still
// loads, since the field couldn't take it anyway.
document.addEventListener('paste', (e) => {
  const f = e.clipboardData?.files?.[0];
  if (!f) return;
  const field = e.target.closest?.('input:not([type=file]), textarea, [contenteditable]');
  if (field && e.clipboardData.types.includes('text/plain')) return;
  e.preventDefault();
  chooseFile(f);
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

$('copies').addEventListener('input', () => {
  const n = wholeCopies($('copies').value);
  if (n === 1) pref.del(PREF_COPIES);
  else if (n > 1 && n <= state.maxCopies) pref.set(PREF_COPIES, String(n));
});

// After a successful print, unless the count was changed while it was sending.
function resetCopies(sent) {
  if ($('copies').value !== sent) return;
  $('copies').value = '1';
  pref.del(PREF_COPIES);
}

// Called once the real copies limit (MAX_COPIES) has come from /api/printer.
function restoreCopies() {
  const n = wholeCopies(pref.get(PREF_COPIES) || '');
  if (n > 1 && n <= state.maxCopies) $('copies').value = String(n);
}

// ---------------------------------------------------------------- feed a label

// One ordinary zpl job through lprint, so it lands in the jobs table like a
// print. The server holds the zpl; the page only names the action. The result
// shows as a badge in the printer status, which is already a live region.
let feedTimer;
function feedStatus(tone, text) {
  clearTimeout(feedTimer);
  const s = $('feed-status');
  s.className = `badge badge-${tone} feed-status`;
  s.textContent = s.title = text;
  s.hidden = !text;
}

$('feed').addEventListener('click', async () => {
  const btn = $('feed');
  btn.disabled = true;
  btn.textContent = 'feeding…';
  feedStatus('neutral', '');
  try {
    const r = await api('/api/printer/maintenance', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'feed' }),
    });
    r.jobs.forEach((j) => state.mine.add(j.id));
    const warn = r.warnings?.length ? ` · ${r.warnings.join(', ')}` : '';
    feedStatus('success', `feed sent · job ${r.jobs[0].id}${warn}`);
    feedTimer = setTimeout(() => feedStatus('neutral', ''), 10000);
  } catch (e) {
    feedStatus('danger', `feed failed: ${e.message}`);
  } finally {
    btn.disabled = false;
    btn.textContent = 'feed a label';
    loadJobs();
    loadPrinter();
  }
});

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
  'too-big': () => `that file is over the ${state.maxMB} MB limit`,
  unsupported: () => "labelprint can't print that file — share a zpl, pdf, png or jpeg",
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
    state.maxMB = p.maxUploadMB;
    state.maxCopies = p.maxCopies;
    $('max-mb').textContent = p.maxUploadMB;
    $('copies').max = p.maxCopies;
  }
  restoreCopies();
  updateSummary();
  fillMedia(p || { media: [{ name: 'na_index-4x6_4x6in', label: '4 × 6 in' }], defaultMedia: 'na_index-4x6_4x6in' });
  // after the size limit and media list are known, which the preview needs
  const s = await shared;
  if (s?.file) chooseFile(s.file);
  else if (s?.error) showResult('err', s.error);
  loadJobs();
  setInterval(async () => { const q = await loadPrinter(); if (q) fillMedia(q); }, 30000);
})();

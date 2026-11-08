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
  $('drop').classList.toggle('has-file', !!file);
  $('file-row').hidden = !file;
  $('pages').value = '';
  if (!file) {
    $('file').value = '';
    $('preview').hidden = true;
    $('pages-field').hidden = true;
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

let pagesTimer;
$('pages').addEventListener('input', () => {
  clearTimeout(pagesTimer);
  pagesTimer = setTimeout(preview, 450);
});

async function preview() {
  if (!state.file) return;
  const seq = ++state.previewSeq;
  const box = $('preview');
  box.classList.add('loading');
  $('pages').removeAttribute('aria-invalid');
  let r;
  try {
    r = await api('/api/preview', { method: 'POST', body: formData({ pages: $('pages').value, media: $('media').value }) });
  } catch (e) {
    if (seq !== state.previewSeq) return;
    box.classList.remove('loading');
    if (state.kind === 'pdf' && /page/i.test(e.message)) {
      // a bad page range: keep the file, flag the field
      $('pages').setAttribute('aria-invalid', 'true');
      $('pages-hint').textContent = e.message;
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
  box.classList.remove('loading');
  $('pages-hint').textContent = 'e.g. 1-3, 5';
  state.kind = r.kind;
  $('file-kind').textContent = r.kind;
  $('file-kind').className = 'badge badge-accent';
  $('pages-field').hidden = r.kind !== 'pdf';
  setMediaHint(r.kind);
  renderPreview(r);
  updateSummary(r);
}

function renderPreview(r) {
  const grid = $('preview-grid');
  const note = $('preview-note');
  grid.replaceChildren();
  document.querySelector('.zpl-source')?.remove();
  $('preview').hidden = false;
  if (r.kind === 'pdf') {
    const shown = r.pages.length;
    note.textContent = `${r.selected} of ${r.pageCount} page${r.pageCount === 1 ? '' : 's'} selected · ` +
      `rendered at 203 dpi, grayscale` + (r.selected > shown ? ` · showing the first ${shown}` : '');
    for (const p of r.pages) {
      grid.append(el('figure', { className: 'preview-item' },
        el('img', { src: p.image, alt: `page ${p.page}`, loading: 'lazy' }),
        el('figcaption', { textContent: `p${p.page}` })));
    }
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

// the zpl itself, collapsed unless there's no picture to look at
function zplSource(open) {
  const pre = el('pre', { className: 'preview-zpl', tabIndex: 0 });
  const details = el('details', { className: 'zpl-source', open }, el('summary', { textContent: 'zpl source' }), pre);
  $('preview-grid').after(details);
  state.file.slice(0, 4096).text().then((t) => {
    pre.textContent = t + (state.file.size > 4096 ? '\n…' : '');
  });
}

function updateSummary(r) {
  const btn = $('print');
  const copies = parseInt($('copies').value, 10);
  const copiesOK = copies >= 1 && copies <= state.maxCopies;
  if (copiesOK) $('copies').removeAttribute('aria-invalid');
  else $('copies').setAttribute('aria-invalid', 'true');
  btn.disabled = !state.kind || !copiesOK;
  const sum = $('summary');
  if (!state.file) { sum.textContent = 'choose a file to start'; return; }
  if (!state.kind) { sum.textContent = ''; return; }
  if (!copiesOK) { sum.textContent = `copies: 1 to ${state.maxCopies}`; return; }
  let units = 1;
  let what = 'label';
  if (state.kind === 'pdf') {
    units = r?.selected ?? state.lastSelected ?? 1;
    state.lastSelected = units;
    what = 'page';
  }
  const total = units * copies;
  sum.textContent = copies > 1 && units > 1
    ? `${units} ${what}s × ${copies} copies = ${total} labels`
    : `${total} label${total === 1 ? '' : 's'}`;
}
$('copies').addEventListener('input', () => updateSummary());
$('media').addEventListener('change', () => { if (state.kind === 'zpl') preview(); });

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
  const fields = { copies: $('copies').value, pages: $('pages').value };
  if (state.kind !== 'zpl') fields.media = $('media').value;
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
    $('jobs-body').replaceChildren(el('tr', {}, el('td', { colSpan: 4, className: 'muted', textContent: e.message })));
  }
  // poll quickly while something is in flight, lazily otherwise
  jobsTimer = setTimeout(loadJobs, busy ? 2000 : 20000);
}

function renderJobs(jobs) {
  const body = $('jobs-body');
  if (!jobs.length) {
    body.replaceChildren(el('tr', {}, el('td', { colSpan: 4, className: 'muted', textContent: 'no jobs yet' })));
    return;
  }
  body.replaceChildren(...jobs.map((j) => {
    const name = el('td', { className: 'name', textContent: j.name || 'untitled' });
    const sub = [j.user, j.state !== 'completed' ? j.message : ''].filter(Boolean).join(' · ');
    if (sub) name.append(el('span', { className: 'sub', textContent: sub }));
    const when = el('td', { className: 'when', textContent: ago(j.completed || j.created) });
    if (j.created) when.title = new Date(j.created).toLocaleString();
    return el('tr', { className: state.mine.has(j.id) ? 'mine' : '' },
      el('td', { className: 'num', textContent: j.id }),
      name,
      el('td', {}, el('span', { className: 'badge ' + (stateBadge[j.state] || 'badge-neutral'), textContent: j.state })),
      when);
  }));
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

// ---------------------------------------------------------------- theme

$('theme').addEventListener('click', () => {
  const next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  try { localStorage.setItem('owine-theme', next); } catch { /* storage blocked */ }
});

// ---------------------------------------------------------------- boot

(async () => {
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
  loadJobs();
  setInterval(async () => { const q = await loadPrinter(); if (q) fillMedia(q); }, 30000);
})();

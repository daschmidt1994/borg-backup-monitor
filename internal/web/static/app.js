// Borg Backup Monitor – frontend without build step.
// All data is inserted via textContent (no HTML from the server).

const $ = (sel, root = document) => root.querySelector(sel);

// ---------- DOM helpers ----------
function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === null || kid === undefined || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// Icons: own simple line drawings (constant markup, no user data).
const ICON_PATHS = {
  ok: '<circle cx="12" cy="12" r="9"/><path d="M8 12.5l2.7 2.7L16.5 9"/>',
  warning: '<path d="M12 3.5L2.8 19.5h18.4L12 3.5z"/><path d="M12 10v4.5"/><circle cx="12" cy="17" r=".6" fill="currentColor"/>',
  error: '<path d="M8.2 3h7.6L21 8.2v7.6L15.8 21H8.2L3 15.8V8.2z"/><path d="M9.2 9.2l5.6 5.6M14.8 9.2l-5.6 5.6"/>',
  overdue: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5.2l3.4 2"/><path d="M19.5 4.5l1.8-1.8"/>',
  unknown: '<circle cx="12" cy="12" r="9"/><path d="M9.6 9.3a2.5 2.5 0 1 1 3.6 2.3c-.8.4-1.2 1-1.2 1.9v.4"/><circle cx="12" cy="16.8" r=".6" fill="currentColor"/>',
  running: '<path d="M20 12a8 8 0 1 1-2.3-5.7"/><path d="M20 4.5v4h-4"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5.5"/><circle cx="12" cy="7.8" r=".6" fill="currentColor"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5.2l3.4 2"/>',
  refresh: '<path d="M20 12a8 8 0 1 1-2.3-5.7"/><path d="M20 4.5v4h-4"/>',
  shield: '<path d="M12 3l7.5 3v5.5c0 4.6-3.2 8-7.5 9.5-4.3-1.5-7.5-4.9-7.5-9.5V6z"/><path d="M8.6 12.2l2.3 2.3 4.5-4.6"/>',
  folder: '<path d="M3.5 6.5h6l2 2h9v10h-17z"/>',
  file: '<path d="M6 3h8l4 4v14H6z"/><path d="M14 3v4h4"/>',
  link: '<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  moon: '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>',
  copy: '<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h3"/>',
  archive: '<rect x="3" y="4" width="18" height="5" rx="1"/><path d="M5 9v10h14V9M10 13h4"/>',
  none: '<path d="M7 12h10"/>',
};
function icon(name, label) {
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  s.setAttribute('viewBox', '0 0 24 24');
  s.setAttribute('fill', 'none');
  s.setAttribute('stroke', 'currentColor');
  s.setAttribute('stroke-width', '1.8');
  s.setAttribute('stroke-linecap', 'round');
  s.setAttribute('stroke-linejoin', 'round');
  if (label) { s.setAttribute('role', 'img'); s.setAttribute('aria-label', label); } else s.setAttribute('aria-hidden', 'true');
  s.innerHTML = ICON_PATHS[name] || ICON_PATHS.info; // constant markup only
  return s;
}

// ---------- vocabulary ----------
const LEVEL = {
  ok: { label: 'OK', icon: 'ok', rank: 0 },
  warning: { label: 'Warnung', icon: 'warning', rank: 3 },
  error: { label: 'Fehler', icon: 'error', rank: 5 },
  overdue: { label: 'Überfällig', icon: 'overdue', rank: 4 },
  unknown: { label: 'Unbekannt', icon: 'unknown', rank: 2 },
  info: { label: 'Hinweis', icon: 'info', rank: 1 },
};
const RESULT = {
  success: { label: 'erfolgreich', icon: 'ok', cls: 'lv-ok', sym: '✓' },
  warning: { label: 'mit Warnungen', icon: 'warning', cls: 'lv-warning', sym: '!' },
  failure: { label: 'fehlgeschlagen', icon: 'error', cls: 'lv-error', sym: '✕' },
  running: { label: 'läuft', icon: 'running', cls: 'lv-running', sym: '↻' },
  archive: { label: 'nur Archiv bekannt', icon: 'archive', cls: 'archive', sym: '▪' },
  none: { label: 'kein Lauf', icon: 'none', cls: 'none', sym: '–' },
};
const RESTORE = {
  passed: { label: 'bestanden', cls: 'lv-ok', icon: 'shield' },
  'passed-unverified': { label: 'bestanden, ohne Referenz', cls: 'lv-warning', icon: 'shield' },
  stale: { label: 'veraltet', cls: 'lv-overdue', icon: 'overdue' },
  never: { label: 'nie getestet', cls: 'lv-unknown', icon: 'unknown' },
  failed: { label: 'fehlgeschlagen', cls: 'lv-error', icon: 'error' },
  running: { label: 'läuft', cls: 'lv-running', icon: 'running' },
  disabled: { label: 'nicht eingerichtet', cls: 'lv-info', icon: 'info' },
};

function badge(levelKey, text) {
  const l = LEVEL[levelKey] || LEVEL.unknown;
  return h('span', { class: `badge lv-${levelKey}` }, icon(l.icon), text || l.label);
}
function resultBadge(res) {
  const r = RESULT[res] || RESULT.none;
  return h('span', { class: `badge ${r.cls}` }, icon(r.icon), r.label);
}
function restoreBadge(state) {
  const r = RESTORE[state] || RESTORE.never;
  return h('span', { class: `badge ${r.cls}` }, icon(r.icon), r.label);
}

// ---------- formatting ----------
const dtf = new Intl.DateTimeFormat('de-DE', { day: '2-digit', month: '2-digit', year: 'numeric', hour: '2-digit', minute: '2-digit' });
const nf1 = new Intl.NumberFormat('de-DE', { maximumFractionDigits: 1 });
const nf0 = new Intl.NumberFormat('de-DE');
const fmtDate = (iso) => (iso ? dtf.format(new Date(iso)) : '–');
function dur(sec) {
  if (sec === null || sec === undefined) return 'unbekannt';
  sec = Math.abs(sec);
  if (sec < 60) return `${Math.round(sec)} Sek.`;
  if (sec < 3600) return `${Math.round(sec / 60)} Min.`;
  if (sec < 48 * 3600) {
    const h = Math.floor(sec / 3600), m = Math.round((sec % 3600) / 60);
    return m && h < 10 ? `${h} Std. ${m} Min.` : `${h} Std.`;
  }
  const d = Math.floor(sec / 86400), hh = Math.round((sec % 86400) / 3600);
  return hh && d < 7 ? `${d} Tage ${hh} Std.` : `${d} Tage`;
}
function interval(sec) {
  if (sec % 604800 === 0) return sec === 604800 ? '1 Woche' : `${sec / 604800} Wochen`;
  if (sec % 86400 === 0) return sec === 86400 ? '1 Tag' : `${sec / 86400} Tage`;
  return dur(sec);
}
const ago = (iso) => (iso ? `vor ${dur((Date.now() - new Date(iso)) / 1000)}` : '–');
function bytes(n) {
  if (!n) return '–';
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; }
  return `${nf1.format(n)} ${u[i]}`;
}

// ---------- API ----------
async function api(path, opts = {}) {
  const res = await fetch(path, {
    method: opts.method || 'GET',
    credentials: 'same-origin',
    headers: { 'X-Requested-With': 'borg-monitor', ...(opts.body ? { 'Content-Type': 'application/json' } : {}) },
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  if (res.status === 401 && !opts.noAuthRedirect) {
    showLogin();
    throw new Error('Bitte anmelden');
  }
  let data = null;
  try { data = await res.json(); } catch { /* empty */ }
  if (!res.ok) throw new Error((data && data.error) || `Fehler ${res.status}`);
  return data;
}

function toast(msg) {
  const t = h('div', { class: 'toast', role: 'status', text: msg });
  document.body.append(t);
  setTimeout(() => t.remove(), 4000);
}

// ---------- theme ----------
function storedTheme() { try { return localStorage.getItem('bbm-theme'); } catch { return null; } }
function applyTheme(t) {
  if (t) document.documentElement.dataset.theme = t; else delete document.documentElement.dataset.theme;
  const dark = t ? t === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
  const b = $('#theme-btn');
  b.replaceChildren(icon(dark ? 'sun' : 'moon'));
}
$('#theme-btn').addEventListener('click', () => {
  const cur = document.documentElement.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  const next = cur === 'dark' ? 'light' : 'dark';
  try { localStorage.setItem('bbm-theme', next); } catch { /* private mode */ }
  applyTheme(next);
});
applyTheme(storedTheme());

// ---------- app state ----------
const state = { ov: null, filter: { text: '', level: '', host: '', restore: '' }, sort: 'urgency', timer: null };

function setDemo(on) { $('#demo-banner').hidden = !on; }

function showLogin(msg) {
  clearInterval(state.timer);
  $('#topbar-right').hidden = true;
  const err = h('div', { class: 'error-msg', role: 'alert', text: msg || '' });
  const user = h('input', { name: 'user', autocomplete: 'username', required: true, 'aria-label': 'Benutzername', placeholder: 'Benutzername' });
  const pw = h('input', { name: 'password', type: 'password', autocomplete: 'current-password', required: true, 'aria-label': 'Passwort', placeholder: 'Passwort' });
  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      try {
        const r = await api('/api/login', { method: 'POST', body: { user: user.value, password: pw.value }, noAuthRedirect: true });
        setDemo(r.demo);
        start();
      } catch (ex) { err.textContent = ex.message; pw.value = ''; pw.focus(); }
    },
  }, user, pw, h('button', { class: 'btn primary', type: 'submit', text: 'Anmelden' }), err);
  $('#view').replaceChildren(h('div', { class: 'card login' },
    h('h1', { text: 'Anmelden' }),
    h('p', { class: 'muted small', text: 'Monitoring der Borg-/borgmatic-Backups. Nur lesender Zugriff – abgesehen vom optionalen Restore-Test.' }),
    form,
    h('p', { class: 'muted small', id: 'demo-hint', hidden: true, text: 'Demo-Modus: Benutzer „demo“, Passwort „demo“.' })));
  user.focus();
  fetch('/api/public').then((r) => r.json()).then((p) => { setDemo(p.demo); $('#demo-hint').hidden = !p.demo; }).catch(() => {});
}

async function start() {
  try {
    const me = await api('/api/me', { noAuthRedirect: true });
    setDemo(me.demo);
  } catch { showLogin(); return; }
  $('#topbar-right').hidden = false;
  route();
  clearInterval(state.timer);
  state.timer = setInterval(() => { if (!document.hidden) route(true); }, 60000);
}

$('#logout-btn').addEventListener('click', async () => {
  try { await api('/api/logout', { method: 'POST' }); } catch { /* ignore */ }
  showLogin('Abgemeldet.');
});

window.addEventListener('hashchange', () => route());

function route(background = false) {
  const m = location.hash.match(/^#\/repo\/([0-9a-f]+)/);
  if (m) renderDetail(m[1], background); else renderDashboard(background);
}

function updateFreshness(ov) {
  const f = $('#freshness');
  const oldest = ov.summary.oldest_update;
  f.replaceChildren(icon('clock'), h('span', { class: 'long', text: 'Seite: ' + fmtDate(ov.generated_at) + ' · ' }),
    h('span', { text: oldest ? `älteste Grundlage ${ago(oldest)}` : 'noch keine Daten' }));
}

// ---------- dashboard ----------
async function renderDashboard(background) {
  if (!background) $('#view').replaceChildren(h('p', { class: 'muted' }, h('span', { class: 'spinner' }), ' Lade Übersicht …'));
  let ov;
  try { ov = await api('/api/overview'); } catch (e) { if (!background) $('#view').replaceChildren(h('div', { class: 'card', text: e.message })); return; }
  state.ov = ov;
  setDemo(ov.demo);
  updateFreshness(ov);
  document.title = titleFor(ov);
  const scroll = window.scrollY;
  $('#view').replaceChildren(h('div', { class: 'stack' },
    overallCard(ov), statusCards(ov), restoreCards(ov), tableCard(ov), historyCard(ov)));
  if (background) window.scrollTo(0, scroll);
}

function titleFor(ov) {
  const l = ov.summary.levels;
  const bad = (l.error || 0) + (l.overdue || 0);
  return bad ? `(${bad}) Borg Backup Monitor` : 'Borg Backup Monitor';
}

function overallCard(ov) {
  const l = ov.summary.levels, total = ov.summary.total;
  const bad = (l.error || 0) + (l.overdue || 0), warn = (l.warning || 0) + (l.unknown || 0);
  let lvl = 'ok', title = `Alle ${total} Backups sind aktuell und fehlerfrei.`;
  if (bad) { lvl = l.error ? 'error' : 'overdue'; title = `${bad} von ${total} Backups fehlen oder haben Fehler.`; }
  else if (warn) { lvl = l.warning ? 'warning' : 'unknown'; title = `${warn} von ${total} Backups brauchen Aufmerksamkeit.`; }
  if (!total) { lvl = 'unknown'; title = 'Noch keine Backups konfiguriert.'; }
  const s = ov.summary;
  const notes = [];
  if (s.newest_update) notes.push(`Neueste Information ${ago(s.newest_update)}, älteste zugrunde liegende ${ago(s.oldest_update)}`);
  const tested = (s.restore.passed || 0) + (s.restore['passed-unverified'] || 0);
  notes.push(`Wiederherstellung aktuell getestet: ${tested} von ${total}`);
  const borg = ov.borg || {};
  const extra = [];
  if (borg.error) extra.push(h('div', { class: 'notice warn', role: 'alert' }, h('strong', { text: 'Borg nicht nutzbar: ' }), borg.error));
  return h('section', { class: 'card', 'aria-label': 'Gesamtstatus' },
    h('div', { class: `overall lv-${lvl}` }, h('span', { class: 'big-icon' }, icon(LEVEL[lvl].icon, LEVEL[lvl].label)),
      h('div', {}, h('h1', { text: title }), h('p', { text: notes.join(' · ') }),
        h('p', { class: 'small', text: `Borg ${borg.version || 'unbekannt'}${borg.bypass_lock ? ' · liest ohne Repository-Sperre (--bypass-lock)' : ''}` }))),
    ...extra);
}

function statusCards(ov) {
  const order = ['ok', 'warning', 'error', 'overdue', 'unknown'];
  const sub = { ok: 'aktuell und fehlerfrei', warning: 'Warnungen oder unbestätigt', error: 'fehlgeschlagen / nicht erreichbar', overdue: 'älter als erwartet', unknown: 'keine oder veraltete Daten' };
  return h('section', { 'aria-label': 'Status nach Kategorie' },
    h('h2', { text: 'Backup-Status' }),
    h('div', { class: 'cards' }, order.map((k) => h('button', {
      class: `scard lv-${k}`, type: 'button', 'aria-pressed': String(state.filter.level === k),
      onclick: () => { state.filter.level = state.filter.level === k ? '' : k; state.filter.restore = ''; renderDashboard(true); },
    }, h('span', { class: 'row' }, icon(LEVEL[k].icon), LEVEL[k].label),
      h('span', { class: 'num', text: ov.summary.levels[k] || 0 }), h('span', { class: 'sub', text: sub[k] })))));
}

function restoreCards(ov) {
  const order = ['passed', 'passed-unverified', 'stale', 'never', 'failed'];
  const sub = { passed: 'gegen Referenz geprüft', 'passed-unverified': 'Inhalt nicht unabhängig geprüft', stale: 'Test zu alt', never: 'noch kein Test', failed: 'letzter Test fehlgeschlagen' };
  return h('section', { 'aria-label': 'Restore-Tests' },
    h('h2', { text: 'Wiederherstellung getestet' }),
    h('div', { class: 'cards restore-cards' }, order.map((k) => {
      const r = RESTORE[k];
      return h('button', {
        class: `scard ${r.cls}`, type: 'button', 'aria-pressed': String(state.filter.restore === k),
        onclick: () => { state.filter.restore = state.filter.restore === k ? '' : k; state.filter.level = ''; renderDashboard(true); },
      }, h('span', { class: 'row' }, icon(r.icon), r.label), h('span', { class: 'num', text: ov.summary.restore[k] || 0 }), h('span', { class: 'sub', text: sub[k] }));
    })),
    h('p', { class: 'muted small', text: 'Getrennt vom Backup-Status: ein erfolgreiches Backup sagt nichts darüber, ob die Wiederherstellung funktioniert.' }));
}

function filteredRows(ov) {
  const f = state.filter, q = f.text.trim().toLowerCase();
  let rows = ov.rows.filter((r) => (!f.level || r.status.level === f.level) && (!f.host || r.host === f.host) &&
    (!f.restore || r.restore.state === f.restore) &&
    (!q || `${r.host} ${r.job} ${r.repo} ${r.description || ''} ${r.location || ''}`.toLowerCase().includes(q)));
  const by = {
    urgency: (a, b) => LEVEL[b.status.level].rank - LEVEL[a.status.level].rank || a.host.localeCompare(b.host),
    host: (a, b) => `${a.host}/${a.job}/${a.repo}`.localeCompare(`${b.host}/${b.job}/${b.repo}`),
    age: (a, b) => (b.age_seconds ?? 1e12) - (a.age_seconds ?? 1e12),
    restore: (a, b) => new Date(a.restore.last?.finished_at || 0) - new Date(b.restore.last?.finished_at || 0),
  };
  return rows.sort(by[state.sort] || by.urgency);
}

function tableCard(ov) {
  const hosts = [...new Set(ov.rows.map((r) => r.host))].sort();
  const tbody = h('tbody');
  const count = h('span', { class: 'muted small' });
  const fill = () => {
    const rows = filteredRows(ov);
    count.textContent = `${rows.length} von ${ov.rows.length}`;
    tbody.replaceChildren(...(rows.length ? rows.map(rowEl) : [h('tr', {}, h('td', { colspan: 8, class: 'empty', text: 'Keine Einträge für diesen Filter.' }))]));
  };
  const search = h('input', { type: 'search', placeholder: 'Suchen: Host, Job, Repository …', 'aria-label': 'Suchen', value: state.filter.text,
    oninput: (e) => { state.filter.text = e.target.value; fill(); } });
  const lvl = h('select', { 'aria-label': 'Status filtern', onchange: (e) => { state.filter.level = e.target.value; renderDashboard(true); } },
    h('option', { value: '', text: 'Alle Status' }), Object.entries(LEVEL).filter(([k]) => k !== 'info').map(([k, v]) => h('option', { value: k, selected: state.filter.level === k, text: v.label })));
  const host = h('select', { 'aria-label': 'Host filtern', onchange: (e) => { state.filter.host = e.target.value; fill(); } },
    h('option', { value: '', text: 'Alle Hosts' }), hosts.map((x) => h('option', { value: x, selected: state.filter.host === x, text: x })));
  const sort = h('select', { 'aria-label': 'Sortierung', onchange: (e) => { state.sort = e.target.value; fill(); } },
    [['urgency', 'Nach Dringlichkeit'], ['host', 'Nach Host'], ['age', 'Ältestes Backup zuerst'], ['restore', 'Ältester Restore-Test zuerst']]
      .map(([v, t]) => h('option', { value: v, selected: state.sort === v, text: t })));
  const reset = (state.filter.level || state.filter.restore || state.filter.host || state.filter.text)
    ? h('button', { class: 'btn ghost', type: 'button', text: 'Filter zurücksetzen', onclick: () => { state.filter = { text: '', level: '', host: '', restore: '' }; renderDashboard(true); } }) : null;
  const heads = ['Status', 'Host / Job', 'Repository', 'Letzter Erfolg', 'Letzter Versuch', 'Archive', 'Größe', 'Restore-Test'];
  fill();
  return h('section', { class: 'card', 'aria-label': 'Backups' },
    h('div', { class: 'toolbar' }, search, lvl, host, sort, reset, count),
    h('div', { class: 'table-wrap' }, h('table', { class: 'responsive' },
      h('thead', {}, h('tr', {}, heads.map((t) => h('th', { scope: 'col', text: t })))), tbody)));
}

function cell(label, ...kids) { return h('td', { 'data-label': label }, h('div', {}, ...kids)); }
function line2(text) { return h('div', { class: 'line2', text }); }

function rowEl(r) {
  const open = () => { location.hash = `#/repo/${r.repo_id}`; };
  const firstReason = (r.status.reasons.find((x) => x.level !== 'info') || r.status.reasons[0] || {}).text || '';
  const la = r.running || r.last_attempt;
  const a = r.archives;
  let archives;
  if (!r.queried) archives = [h('span', { class: 'muted', text: 'nicht abgefragt' })];
  else if (!a.checked) archives = [h('span', { class: 'muted', text: 'noch nicht abgefragt' })];
  else archives = [h('div', { text: nf0.format(a.count) + ' Archive' }),
    line2(a.latest ? `neuestes ${fmtDate(a.latest.start)}` : 'keine Archive'),
    a.ok ? null : h('div', { class: 'line2 err', text: '⚠ Abfrage fehlgeschlagen' })];
  const sz = a.sizes;
  return h('tr', { class: 'clickable', tabindex: 0, onclick: open, onkeydown: (e) => { if (e.key === 'Enter') open(); } },
    cell('Status', badge(r.status.level), h('div', { class: 'line2 clamp', title: firstReason, text: firstReason }),
      h('div', { class: 'line2 nowrap', title: r.status.updated_at ? 'Daten aktualisiert: ' + fmtDate(r.status.updated_at) : '', text: r.status.updated_at ? `Daten ${ago(r.status.updated_at)}` : 'keine Daten' })),
    cell('Host / Job', h('strong', { text: r.host }), line2(r.job + (r.description ? ` – ${r.description}` : ''))),
    cell('Repository', h('div', { text: r.repo }), r.location ? h('div', { class: 'line2 mono ellipsis', title: r.location, text: r.location }) : null),
    cell('Letzter Erfolg', r.last_success_at ? [h('div', { class: 'nowrap', text: ago(r.last_success_at) }), line2(fmtDate(r.last_success_at) + (r.success_source === 'archive' ? ' · nur Archiv belegt' : ''))]
      : [h('span', { class: 'muted', text: 'unbekannt' })],
      h('div', { class: 'line2 nowrap', title: `Toleranz ${dur(r.tolerance_seconds)}`, text: `erwartet alle ${interval(r.interval_seconds)}` })),
    cell('Letzter Versuch', la ? [resultBadge(la.result), line2((la.result === 'running' ? 'seit ' + fmtDate(la.started_at) : fmtDate(la.finished_at || la.received_at)) +
      (la.result !== 'running' ? ` · Dauer ${dur(la.duration_seconds)}` : ''))] : [h('span', { class: 'muted', text: r.ping_configured ? 'keine Meldung' : 'nicht gemeldet' })]),
    cell('Archive', archives),
    cell('Größe', sz ? [h('div', { class: 'nowrap', text: bytes(sz.original) }), line2(`dedupliziert ${bytes(sz.deduplicated)}`)] : [h('span', { class: 'muted', text: 'unbekannt' })]),
    cell('Restore-Test', restoreBadge(r.restore.state), r.restore.last ? line2(fmtDate(r.restore.last.finished_at || r.restore.last.started_at)) : null));
}

function dayCell(c) {
  const r = RESULT[c.result] || RESULT.none;
  const label = `${new Date(c.date).toLocaleDateString('de-DE', { weekday: 'short', day: '2-digit', month: '2-digit' })}: ${r.label}${c.runs > 1 ? ` (${c.runs} Läufe)` : ''}`;
  return h('span', { class: `day ${r.cls}`, title: label, role: 'img', 'aria-label': label, text: r.sym });
}

function historyCard(ov) {
  const rows = filteredRows(ov);
  const dates = rows[0]?.history.map((c) => c.date) || [];
  return h('section', { class: 'card', 'aria-label': 'Verlauf' },
    h('h2', { text: 'Verlauf der letzten 14 Tage' }),
    h('div', { class: 'hist' },
      h('span'), h('div', { class: 'days-head' }, dates.map((d, i) => h('span', { text: i % 2 === 1 || i === 13 ? new Date(d).getDate() + '.' : '' }))),
      rows.map((r) => [h('div', { class: 'label' }, h('a', { href: `#/repo/${r.repo_id}`, text: `${r.host} / ${r.job} → ${r.repo}` })),
        h('div', { class: 'days' }, r.history.map(dayCell))])),
    h('div', { class: 'legend' }, ['success', 'warning', 'failure', 'running', 'archive', 'none'].map((k) => h('span', {}, dayCell({ result: k, date: new Date().toISOString(), runs: 0 }), RESULT[k].label))));
}

// ---------- detail ----------
async function renderDetail(id, background) {
  if (!background) $('#view').replaceChildren(h('p', { class: 'muted' }, h('span', { class: 'spinner' }), ' Lade …'));
  if (background && document.querySelector('[data-busy]')) return; // don't disturb the restore wizard
  let d;
  try { d = await api(`/api/repos/${id}`); } catch (e) { $('#view').replaceChildren(h('div', { class: 'card' }, e.message, ' ', h('a', { href: '#/', text: 'Zur Übersicht' }))); return; }
  const r = d.row;
  document.title = `${r.status.label}: ${r.host}/${r.job}/${r.repo} – Borg Backup Monitor`;
  const scroll = window.scrollY;
  const refreshBtn = r.queried ? h('button', { class: 'btn', type: 'button', onclick: async (e) => {
    e.currentTarget.disabled = true;
    e.currentTarget.replaceChildren(h('span', { class: 'spinner' }), ' Frage ab …');
    try { await api(`/api/repos/${id}/refresh`, { method: 'POST' }); toast('Repository abgefragt.'); } catch (ex) { toast(ex.message); }
    renderDetail(id, true);
  } }, icon('refresh'), 'Repository jetzt abfragen') : null;
  $('#view').replaceChildren(h('div', { class: 'stack' },
    h('div', {}, h('div', { class: 'crumbs' }, h('a', { href: '#/', text: 'Übersicht' }), ` / ${r.host} / ${r.job}`),
      h('div', { class: 'head' }, h('div', {}, h('h1', { text: `${r.host} / ${r.job} → ${r.repo}` }),
        h('div', { class: 'muted small', text: [r.description, r.location].filter(Boolean).join(' · ') })),
        h('div', { class: 'toolbar flush' }, badge(r.status.level), restoreBadge(r.restore.state), refreshBtn))),
    h('div', { class: 'grid2' }, judgementCard(r), factsCard(r)),
    findingsCard(d),
    historyChartCard(d),
    restoreCard(d, id),
    archivesCard(r),
    setupCard(d)));
  if (background) window.scrollTo(0, scroll);
}

function reasonItem(x) {
  const l = LEVEL[x.level] || LEVEL.info;
  return h('li', { class: `lv-${x.level}` }, icon(l.icon, l.label), h('div', {}, h('div', { text: x.text }), x.hint ? h('div', { class: 'hint', text: x.hint }) : null));
}

function judgementCard(r) {
  return h('section', { class: 'card' },
    h('h2', {}, 'Bewertung: ', badge(r.status.level)),
    h('ul', { class: 'reasons' }, r.status.reasons.map(reasonItem)),
    h('h3', { text: 'Worauf die Bewertung beruht' }),
    h('ul', { class: 'basis' }, r.status.basis.map((b) => h('li', {}, h('span', { class: 'src', text: b.source }),
      h('span', {}, b.text, b.at ? h('span', { class: 'muted', text: ` · ${fmtDate(b.at)} (${ago(b.at)})` }) : null)))),
    h('p', { class: 'muted small', text: `Daten zuletzt aktualisiert: ${r.status.updated_at ? fmtDate(r.status.updated_at) + ' (' + ago(r.status.updated_at) + ')' : 'nie'}` }));
}

function fact(k, v, s) { return h('div', { class: 'fact' }, h('div', { class: 'k', text: k }), h('div', { class: 'v' }, v), s ? h('div', { class: 's', text: s }) : null); }

function factsCard(r) {
  const la = r.last_finished, run = r.running, a = r.archives, sz = a.sizes;
  return h('section', { class: 'card' }, h('h2', { text: 'Eckdaten' }), h('div', { class: 'facts' },
    fact('Letzter Versuch', la ? resultBadge(la.result) : 'keiner gemeldet', la ? `${fmtDate(la.finished_at || la.received_at)} · Dauer ${dur(la.duration_seconds)}${la.exit_code !== undefined && la.exit_code !== null ? ' · Exit-Code ' + la.exit_code : ''}` : null),
    run ? fact('Läuft gerade', resultBadge('running'), `seit ${fmtDate(run.started_at)} (${ago(run.started_at)})`) : null,
    fact('Letztes erfolgreiches Backup', r.last_success_at ? ago(r.last_success_at) : 'unbekannt',
      r.last_success_at ? `${fmtDate(r.last_success_at)}${r.success_source === 'archive' ? ' – nur aus dem Archiv abgeleitet, Lauf-Ergebnis unbekannt' : ''}${r.last_ok?.duration_seconds ? ' · Dauer ' + dur(r.last_ok.duration_seconds) : ''}` : null),
    fact('Erwartetes Intervall', 'alle ' + interval(r.interval_seconds), `Toleranz ${dur(r.tolerance_seconds)} – nur für das Monitoring`),
    fact('Archive', r.queried ? (a.checked ? nf0.format(a.count) : 'noch nicht abgefragt') : 'nicht abgefragt',
      a.latest ? `neuestes: ${a.latest.name} (${fmtDate(a.latest.start)})` : null),
    fact('Repository-Abfrage', !r.queried ? 'abgeschaltet' : !a.checked ? 'ausstehend' : a.ok ? 'erfolgreich' : 'fehlgeschlagen',
      a.checked_at ? `${fmtDate(a.checked_at)}${!a.ok && a.last_good_at ? ' · zuletzt erfolgreich ' + fmtDate(a.last_good_at) : ''}` : null),
    fact('Größe (letztes Archiv)', sz ? bytes(sz.original) : 'unbekannt',
      sz ? `komprimiert ${bytes(sz.compressed)} · dedupliziert ${bytes(sz.deduplicated)}${sz.all_deduplicated ? ' · Repository gesamt ' + bytes(sz.all_deduplicated) : ''} · Quelle: ${sz.source}` : a.sizes_note),
    a.encryption ? fact('Verschlüsselung', a.encryption) : null));
}

function findingEl(f) {
  const lvl = f.level === 'error' ? 'error' : f.level === 'warning' ? 'warning' : 'info';
  return h('div', { class: `finding lv-${lvl}` }, h('div', { class: 't' }, icon(LEVEL[lvl].icon), f.summary),
    f.hint ? h('div', { class: 'h', text: f.hint }) : null, f.detail ? h('pre', { text: f.detail }) : null);
}

function logDetails(runId, label = 'Original-Log anzeigen') {
  const pre = h('pre', { text: 'Lade …' });
  let loaded = false;
  return h('details', { class: 'log', ontoggle: async (e) => {
    if (!e.target.open || loaded) return;
    loaded = true;
    try { const l = await api(`/api/runs/${runId}/log`); pre.textContent = (l.cut ? '… (Anfang gekürzt)\n' : '') + (l.log || '(leer – borgmatic hat kein Log mitgeschickt)'); } catch (ex) { pre.textContent = ex.message; }
  } }, h('summary', { text: label }), pre);
}

function findingsCard(d) {
  const r = d.row;
  const run = r.last_finished;
  const items = [];
  if (r.archives.error) items.push(h('h3', { text: 'Repository-Abfrage' }), findingEl(r.archives.error));
  if (run) {
    items.push(h('h3', { text: `Letzter Lauf (${fmtDate(run.finished_at || run.received_at)})` }));
    if (run.findings?.length) items.push(...run.findings.map(findingEl));
    else items.push(h('p', { class: 'muted', text: 'Keine Fehler oder Warnungen gemeldet.' }));
    if (run.has_log) items.push(logDetails(run.id));
  }
  if (!items.length) items.push(h('p', { class: 'muted', text: 'Keine Meldungen vorhanden.' }));
  return h('section', { class: 'card' }, h('h2', { text: 'Fehler und Warnungen' }), ...items);
}

function chartSVG(runs) {
  const list = runs.slice(0, 40).reverse();
  const W = 760, H = 210, padL = 46, padB = 26, padT = 18;
  const NS = 'http://www.w3.org/2000/svg';
  const el = (t, a, text) => { const e = document.createElementNS(NS, t); for (const [k, v] of Object.entries(a)) e.setAttribute(k, v); if (text !== undefined) e.textContent = text; return e; };
  const svg = el('svg', { viewBox: `0 0 ${W} ${H}`, class: 'chart', role: 'img', 'aria-label': 'Dauer und Ergebnis der letzten Läufe' });
  if (!list.length) { svg.append(el('text', { x: W / 2, y: H / 2, 'text-anchor': 'middle' }, 'Noch keine Läufe gemeldet')); return svg; }
  const maxD = Math.max(60, ...list.map((r) => r.duration_seconds || 0));
  const plotH = H - padB - padT, bw = (W - padL - 10) / list.length;
  svg.append(el('line', { x1: padL, y1: H - padB, x2: W - 5, y2: H - padB, class: 'axis' }));
  svg.append(el('text', { x: padL - 6, y: padT + 4, 'text-anchor': 'end' }, dur(maxD)));
  svg.append(el('text', { x: padL - 6, y: H - padB, 'text-anchor': 'end' }, '0'));
  list.forEach((r, i) => {
    const d = r.duration_seconds;
    const hgt = d ? Math.max(3, (d / maxD) * plotH) : 10;
    const x = padL + i * bw + bw * 0.15, y = H - padB - hgt;
    const g = el('g', {});
    const res = RESULT[r.result] || RESULT.none;
    g.append(el('title', {}, `${fmtDate(r.started_at || r.finished_at || r.received_at)} – ${res.label}, Dauer ${dur(d)}${r.exit_code !== undefined && r.exit_code !== null ? ', Exit-Code ' + r.exit_code : ''}`));
    g.append(el('rect', { x, y, width: Math.max(2, bw * 0.7), height: hgt, rx: 2, class: `bar-${r.result}`, opacity: d ? 1 : 0.45 }));
    if (r.result !== 'success') g.append(el('text', { x: x + bw * 0.35, y: y - 4, 'text-anchor': 'middle', class: `sym sym-${r.result}` }, res.sym));
    svg.append(g);
  });
  [0, Math.floor(list.length / 2), list.length - 1].forEach((i) => {
    const r = list[i];
    const t = r.started_at || r.finished_at || r.received_at;
    svg.append(el('text', { x: padL + i * bw + bw / 2, y: H - 8, 'text-anchor': 'middle' }, new Date(t).toLocaleDateString('de-DE', { day: '2-digit', month: '2-digit' })));
  });
  return svg;
}

function historyChartCard(d) {
  const runs = d.runs || [];
  const body = h('tbody', {}, runs.slice(0, 30).map((r) => h('tr', {},
    h('td', { 'data-label': 'Ergebnis' }, resultBadge(r.result)),
    h('td', { 'data-label': 'Start', class: 'nowrap', text: r.started_at ? fmtDate(r.started_at) : 'unbekannt' }),
    h('td', { 'data-label': 'Ende', class: 'nowrap', text: r.finished_at ? fmtDate(r.finished_at) : (r.result === 'running' ? 'läuft' : '–') }),
    h('td', { 'data-label': 'Dauer', class: 'nowrap', text: dur(r.duration_seconds) }),
    h('td', { 'data-label': 'Exit-Code', text: r.exit_code ?? '–' }),
    h('td', { 'data-label': 'Meldungen' }, r.findings?.length ? r.findings.filter((f) => f.level !== 'info').map((f) => h('div', { class: 'small', text: (f.level === 'error' ? '✕ ' : '! ') + f.summary })) : h('span', { class: 'muted small', text: 'keine' }),
      r.has_log ? logDetails(r.id, 'Log') : null),
  )));
  return h('section', { class: 'card' }, h('h2', { text: 'Verlauf der Backup-Läufe' }),
    chartSVG(runs),
    h('div', { class: 'legend' }, ['success', 'warning', 'failure', 'running'].map((k) => h('span', {}, h('span', { class: `badge ${RESULT[k].cls}` }, icon(RESULT[k].icon), RESULT[k].label)))),
    h('p', { class: 'muted small', text: 'Balkenhöhe = Laufzeit. Blasse Balken: Dauer unbekannt (borgmatic meldet den Start nicht – „start“ in den states eintragen).' }),
    runs.length ? h('div', { class: 'table-wrap' }, h('table', { class: 'responsive' }, h('thead', {}, h('tr', {}, ['Ergebnis', 'Start', 'Ende', 'Dauer', 'Exit-Code', 'Meldungen'].map((t) => h('th', { text: t })))), body)) : null);
}

function archivesCard(r) {
  const a = r.archives;
  if (!r.queried) return h('section', { class: 'card' }, h('h2', { text: 'Archive' }), h('p', { class: 'muted', text: 'Dieses Repository wird nicht abgefragt (query: false).' }));
  return h('section', { class: 'card' }, h('h2', { text: `Archive (${nf0.format(a.count || 0)})` }),
    a.ok ? null : h('div', { class: 'notice warn', text: `Letzte Abfrage fehlgeschlagen – angezeigt wird der Stand vom ${a.last_good_at ? fmtDate(a.last_good_at) : 'unbekannt'}.` }),
    a.recent?.length ? h('div', { class: 'table-wrap' }, h('table', { class: 'responsive' },
      h('thead', {}, h('tr', {}, ['Archiv', 'Start', 'Ende'].map((t) => h('th', { text: t })))),
      h('tbody', {}, a.recent.slice(0, 15).map((x) => h('tr', {}, h('td', { 'data-label': 'Archiv', class: 'mono', text: x.name }),
        h('td', { 'data-label': 'Start', class: 'nowrap', text: fmtDate(x.start) }), h('td', { 'data-label': 'Ende', class: 'nowrap', text: x.end ? fmtDate(x.end) : '–' })))))) : h('p', { class: 'muted', text: 'Keine Archive bekannt.' }));
}

function copyBlock(text) {
  return h('div', { class: 'copy' }, h('pre', { text }), h('button', { class: 'btn', type: 'button', onclick: async () => {
    try { await navigator.clipboard.writeText(text); toast('Kopiert.'); } catch { toast('Kopieren nicht möglich – bitte markieren.'); }
  } }, icon('copy'), 'Kopieren'));
}

function setupCard(d) {
  const r = d.row;
  if (!d.ping_url) return h('section', { class: 'card' }, h('h2', { text: 'Meldungen von borgmatic einrichten' }),
    h('p', { text: 'Für diesen Job ist kein ping_token konfiguriert. Ohne Meldungen kann nicht erkannt werden, ob Läufe fehlerfrei waren – nur, ob Archive existieren.' }),
    h('p', { class: 'muted', text: 'Token erzeugen: docker compose run --rm borg-monitor -new-token – und als ping_token beim Job eintragen.' }));
  return h('section', { class: 'card' }, h('h2', { text: 'Meldungen von borgmatic einrichten' }),
    h('p', { text: 'Der Monitor versteht das Healthchecks-Protokoll, das borgmatic eingebaut hat. borgmatic meldet Start, Erfolg oder Fehler samt Log. Zeitplan und Backup-Konfiguration bleiben in borgmatic.' }),
    h('h3', { text: 'borgmatic ab 1.8 (in der borgmatic-Konfiguration)' }),
    copyBlock(`healthchecks:\n    ping_url: ${d.ping_url}\n    states:\n        - start\n        - finish\n        - fail\n    send_logs: true`),
    h('h3', { text: 'ältere borgmatic-Versionen (unter hooks:)' }),
    copyBlock(`hooks:\n    healthchecks:\n        ping_url: ${d.ping_url}\n        states:\n            - start\n            - finish\n            - fail`),
    h('h3', { text: 'Alternative mit exaktem Exit-Code: Wrapper-Skript (contrib/borgmatic-report.sh)' }),
    copyBlock(`BBM_PING_URL=${d.ping_url} borgmatic-report.sh --verbosity 1 --stats`),
    h('p', { class: 'muted small', text: `Hinweis: Die Adresse enthält ein Geheimnis – nicht öffentlich teilen. Der Job umfasst ${r.repo ? 'alle Repositorys dieser borgmatic-Konfiguration' : ''}.` }));
}

// ---------- restore ----------
function restoreCard(d, id) {
  const r = d.row, rs = r.restore;
  const tests = d.restore_tests || [];
  const list = tests.length ? h('div', { class: 'table-wrap' }, h('table', { class: 'responsive' },
    h('thead', {}, h('tr', {}, ['Ergebnis', 'Zeitpunkt', 'Archiv', 'Auswahl', 'Daten', 'Geprüft', 'Von'].map((t) => h('th', { text: t })))),
    h('tbody', {}, tests.map((t) => h('tr', {},
      h('td', { 'data-label': 'Ergebnis' }, restoreBadge(t.state)),
      h('td', { 'data-label': 'Zeitpunkt', class: 'nowrap', text: fmtDate(t.finished_at || t.started_at) }),
      h('td', { 'data-label': 'Archiv', class: 'mono', text: t.archive }),
      h('td', { 'data-label': 'Auswahl' }, h('div', { class: 'small mono', text: (t.paths || []).join(', ') }), testDetails(t)),
      h('td', { 'data-label': 'Daten', class: 'nowrap', text: `${t.file_count || 0} Einträge · ${bytes(t.bytes)}` }),
      h('td', { 'data-label': 'Geprüft', text: t.verified ? `${t.verified} gegen Referenz` : 'ohne Referenz' }),
      h('td', { 'data-label': 'Von', text: t.started_by || '–' })))))) : h('p', { class: 'muted', text: 'Noch kein Restore-Test für dieses Repository.' });
  return h('section', { class: 'card' },
    h('h2', {}, 'Wiederherstellung: ', restoreBadge(rs.state)),
    h('p', { text: rs.text }),
    h('p', { class: 'muted small', text: 'Getrennt vom Backup-Status bewertet. Ein bestandener Stichprobentest garantiert nicht, dass alle Daten wiederherstellbar sind.' }),
    list,
    r.restore_enabled && r.queried ? wizard(d, id) : h('p', { class: 'muted small', text: r.queried ? 'Restore-Tests sind für diesen Job nicht freigegeben (restore.enabled bzw. restore_test.enabled).' : 'Ohne Repository-Abfrage kein Restore-Test möglich.' }));
}

function testDetails(t) {
  if (!t.files?.length && !t.findings?.length) return null;
  return h('details', { class: 'log' }, h('summary', { text: 'Details' }),
    h('div', { class: 'pad' },
      t.limits ? h('div', { class: 'small muted', text: `Grenzen: ${t.limits}` }) : null,
      h('div', { class: 'small muted', text: `Ziel: ${t.target || '–'}${t.removed ? ' (nach dem Test gelöscht)' : ''}` }),
      (t.findings || []).map(findingEl),
      t.files?.length ? h('table', {}, h('thead', {}, h('tr', {}, ['Pfad', 'Typ', 'Größe', 'Wiederhergestellt', 'Referenz'].map((x) => h('th', { text: x })))),
        h('tbody', {}, t.files.map((f) => h('tr', {}, h('td', { class: 'mono small', text: f.path }), h('td', { text: f.type }), h('td', { text: f.type === 'file' ? bytes(f.size) : '' }),
          h('td', { text: f.restored ? (f.size_ok === false ? '✕ Größe abweichend' : '✓ ja') : '✕ nein' }),
          h('td', { text: { match: '✓ stimmt', mismatch: '✕ weicht ab', missing: '– Original fehlt', 'changed-since-backup': '! Original geändert', '-': '–' }[f.reference || '-'] + (f.reference_by ? ` (${f.reference_by})` : '') + (f.note ? ` – ${f.note}` : '') }))))) : null));
}

function wizard(d, id) {
  const r = d.row;
  const box = h('div', { class: 'mt' });
  const startBtn = h('button', { class: 'btn primary', type: 'button', onclick: () => step1() }, icon('shield'), 'Restore-Test vorbereiten');
  box.append(startBtn);
  const sel = new Map(); // path → item
  let archive = r.archives.recent?.[0]?.name || '';

  const steps = (n) => h('div', { class: 'steps' }, ['1 Archiv', '2 Dateien', '3 Prüfen', '4 Ergebnis'].map((t, i) => h('span', { class: i + 1 === n ? 'on' : '', text: t })));

  function step1() {
    box.dataset.busy = '1';
    if (!r.archives.recent?.length) { box.replaceChildren(h('p', { class: 'notice warn', text: 'Keine Archive bekannt – zuerst das Repository abfragen.' })); return; }
    const s = h('select', { onchange: (e) => { archive = e.target.value; } }, r.archives.recent.map((a) => h('option', { value: a.name, selected: a.name === archive, text: `${a.name} (${fmtDate(a.start)})` })));
    box.replaceChildren(h('div', { class: 'card inner' }, steps(1),
      h('div', { class: 'field' }, h('label', { text: 'Archiv' }), s),
      h('div', { class: 'toolbar' }, h('button', { class: 'btn primary', type: 'button', text: 'Weiter: Dateien wählen', onclick: () => step2('') }), cancelBtn())));
  }
  function cancelBtn() { return h('button', { class: 'btn ghost', type: 'button', text: 'Abbrechen', onclick: () => { delete box.dataset.busy; box.replaceChildren(startBtn); sel.clear(); } }); }

  async function step2(path) {
    const pathIn = h('input', { value: path, placeholder: 'Pfad im Archiv, z. B. etc oder home/anna/Dokumente', 'aria-label': 'Pfad im Archiv' });
    const listBox = h('div', { class: 'filelist' }, h('p', { class: 'muted pad' }, h('span', { class: 'spinner' }), ' Lese Archivinhalt …'));
    const selInfo = h('p', { class: 'small' });
    const updSel = () => {
      let b = 0;
      for (const it of sel.values()) b += it.size || 0;
      selInfo.textContent = sel.size ? `Ausgewählt: ${sel.size} Einträge (${[...sel.keys()].join(', ')}) – Dateien zusammen mind. ${bytes(b)}` : 'Noch nichts ausgewählt.';
      next.disabled = !sel.size;
    };
    const next = h('button', { class: 'btn primary', type: 'button', text: 'Weiter: Auswahl prüfen', onclick: () => step3() });
    box.replaceChildren(h('div', { class: 'card inner' }, steps(2),
      h('p', { class: 'small muted', text: `Archiv: ${archive}. Höchstens ${d.restore_limits.max_files} Einträge und ${bytes(d.restore_limits.max_bytes)}. Ordner anklicken zum Öffnen, Kästchen zum Auswählen.` }),
      h('div', { class: 'toolbar' }, pathIn, h('button', { class: 'btn', type: 'button', text: 'Anzeigen', onclick: () => step2(pathIn.value.trim()) }),
        path ? h('button', { class: 'btn ghost', type: 'button', text: 'Eine Ebene höher', onclick: () => step2(path.split('/').slice(0, -1).join('/')) }) : null),
      listBox, selInfo, h('div', { class: 'toolbar' }, next, cancelBtn())));
    updSel();
    try {
      const res = await api(`/api/repos/${id}/files?archive=${encodeURIComponent(archive)}&path=${encodeURIComponent(path)}`);
      const depth = path ? path.split('/').length + 1 : 1;
      const items = res.items.filter((it) => it.path.split('/').length === depth || it.path === path);
      if (!items.length) { listBox.replaceChildren(h('p', { class: 'muted pad', text: 'Keine Einträge.' })); return; }
      listBox.replaceChildren(...items.map((it) => {
        const cb = h('input', { type: 'checkbox', checked: sel.has(it.path), onchange: (e) => { if (e.target.checked) sel.set(it.path, it); else sel.delete(it.path); updSel(); } });
        const name = it.path === path ? '(dieser Ordner)' : it.path.split('/').pop();
        const nameEl = it.type === 'dir' && it.path !== path ? h('a', { href: '#', onclick: (e) => { e.preventDefault(); step2(it.path); }, text: name + '/' }) : h('span', { text: name });
        return h('label', {}, cb, icon(it.type === 'dir' ? 'folder' : it.type === 'symlink' ? 'link' : 'file'), nameEl,
          it.type === 'symlink' ? h('span', { class: 'muted small', text: `→ ${it.link_target}` }) : null,
          h('span', { class: 'size', text: it.type === 'file' ? bytes(it.size) : it.type === 'dir' ? 'Ordner' : it.type }));
      }), res.truncated ? h('p', { class: 'muted small pad', text: 'Liste gekürzt – Pfad genauer angeben.' }) : '');
    } catch (e) { listBox.replaceChildren(h('p', { class: 'notice warn', text: e.message })); }
  }

  async function step3() {
    box.replaceChildren(h('div', { class: 'card inner' }, steps(3), h('p', {}, h('span', { class: 'spinner' }), ' Prüfe Auswahl und Grenzen …')));
    let p;
    try { p = await api('/api/restore-tests/plan', { method: 'POST', body: { repo_id: id, archive, paths: [...sel.keys()] } }); } catch (e) {
      box.replaceChildren(h('div', { class: 'card inner' }, steps(3), h('p', { class: 'notice warn', text: e.message }), h('div', { class: 'toolbar' }, h('button', { class: 'btn', type: 'button', text: 'Zurück', onclick: () => step2('') }), cancelBtn())));
      return;
    }
    const l = p.limits;
    const confirm = h('input', { type: 'checkbox', id: 'rt-confirm', onchange: (e) => { go.disabled = !e.target.checked || !p.ok; } });
    const go = h('button', { class: 'btn primary', type: 'button', disabled: true, onclick: () => run(p.id) }, icon('shield'), 'Restore-Test starten');
    box.replaceChildren(h('div', { class: 'card plan inner' }, steps(3),
      h('h2', { text: 'Vor dem Start prüfen' }),
      h('dl', {},
        h('dt', { text: 'Repository' }), h('dd', { text: `${r.host} / ${r.job} → ${r.repo}` }),
        h('dt', { text: 'Archiv' }), h('dd', { class: 'mono', text: p.archive + (p.archive_time ? ` (${fmtDate(p.archive_time)})` : '') }),
        h('dt', { text: 'Auswahl' }), h('dd', { class: 'mono', text: p.paths.join(', ') }),
        h('dt', { text: 'Umfang' }), h('dd', { text: `${p.files} Dateien, ${p.items.length} Einträge, ${bytes(p.bytes)}` }),
        h('dt', { text: 'Ziel' }), h('dd', { class: 'mono', text: p.target }),
        h('dt', { text: 'Grenzen' }), h('dd', { text: `max. ${bytes(l.max_bytes)} · ${l.max_files} Einträge · ${dur(l.timeout_seconds)} Laufzeit · Speicher ${bytes(l.memory_max)}${l.memory_enforced ? '' : ' (nicht erzwungen: prlimit fehlt)'}` }),
        h('dt', { text: 'Prüfung gegen' }), h('dd', { text: p.reference })),
      h('div', { class: 'notice mt' }, h('strong', { text: 'Sicherheit: ' }),
        'Wiederhergestellt wird nur in ein neu angelegtes, privates Verzeichnis. Originaldateien und produktive Verzeichnisse werden nicht berührt. Symlinks werden nicht verfolgt, Ausführungsrechte entfernt, nichts wird ausgeführt.'),
      p.problems?.length ? h('div', { class: 'notice warn mt', role: 'alert' }, h('strong', { text: 'Start nicht möglich: ' }), p.problems.join(' · ')) : null,
      h('p', { class: 'small muted', text: p.disclaimer }),
      h('label', { class: 'check', for: 'rt-confirm' }, confirm, h('span', { text: 'Ich habe Auswahl, Ziel und Grenzen geprüft und starte den Test bewusst.' })),
      h('div', { class: 'toolbar' }, go, h('button', { class: 'btn', type: 'button', text: 'Auswahl ändern', onclick: () => step2('') }), cancelBtn())));
  }

  async function run(planId) {
    box.replaceChildren(h('div', { class: 'card inner' }, steps(4), h('p', {}, h('span', { class: 'spinner' }), ' Restore-Test läuft …')));
    let t;
    try { t = await api('/api/restore-tests', { method: 'POST', body: { plan_id: planId, confirm: true } }); } catch (e) {
      box.replaceChildren(h('p', { class: 'notice warn', text: e.message }), cancelBtn()); return;
    }
    while (t.state === 'running') {
      await new Promise((res) => setTimeout(res, 1500));
      try { t = await api(`/api/restore-tests/${t.id}`); } catch { /* keep polling */ }
    }
    delete box.dataset.busy;
    box.replaceChildren(h('div', { class: 'card inner' }, steps(4),
      h('h2', {}, 'Ergebnis: ', restoreBadge(t.state)),
      h('p', { text: `${t.file_count} Einträge, ${bytes(t.bytes)} wiederhergestellt; ${t.verified} Dateien gegen eine unabhängige Referenz geprüft.` }),
      (t.findings || []).map(findingEl), testDetails(t),
      h('div', { class: 'toolbar' }, h('button', { class: 'btn', type: 'button', text: 'Fertig', onclick: () => renderDetail(id) }))));
  }
  return box;
}

// ---------- go ----------
fetch('/api/me', { credentials: 'same-origin' }).then(async (r) => {
  if (r.ok) start(); else showLogin();
}).catch(() => showLogin('Server nicht erreichbar.'));

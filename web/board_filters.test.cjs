const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');

function board(search) {
  const elements = new Map();
  const location = new URL('https://example.test/triage/' + search);
  const document = {
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, {
        value: '', handlers: {}, options: [], hidden: true, focus() {}, querySelector() { return null; },
        addEventListener(event, handle) { this.handlers[event] = handle; },
      });
      return elements.get(id);
    },
    querySelector(selector) { return { focus() { document.focused = selector; } }; },
    addEventListener(event, handle) { this[event] = handle; },
  };
  const el = id => document.getElementById(id);
  const options = {
    catSel: ['REAL_ALL', 'ALL', 'EASY', 'MEDIUM', 'HARD', 'ENHANCEMENT'],
    scopeSel: ['all', 'customer'], subSel: ['all'], sevSel: ['all', 'MAJOR', 'MINOR', 'HIGH_S0_S1'],
    prSel: ['all', 'open_fix'], assSel: ['all'], updWindowSel: ['all', 'custom'],
    sortSel: ['num_desc', 'updated_desc'], prStageSel: ['ALL', 'CI_FAILED'],
    prScopeSel: ['mainline', 'real_bug_mainline', 'all_open'], prBallSel: ['all', 'AUTHOR'],
    prSevSel: ['all'], prSubSel: ['all'], prSortSel: ['prio_desc', 'wait_desc'],
  };
  for (const [id, values] of Object.entries(options)) {
    el(id).options = values.map(value => ({ value }));
    el(id).value = values[0];
  }
  const ctx = vm.createContext({
    document, window: { location, history: { replaceState(_state, _title, url) { location.href = url; } } },
    URL, URLSearchParams, activeView: 'issues',
    renderIssuesView() { ctx.syncUrlState(); }, renderPrsView() { ctx.syncUrlState(); },
    closeEditDrawer() {}, closeInboxDrawer() {}, CSS: { escape: value => value },
    flashAndScrollRow() {}, switchView(view) { ctx.activeView = view; ctx.syncUrlState(); },
  });
  function load(from, to) {
    const start = html.indexOf(from), end = html.indexOf(to, start);
    assert.ok(start >= 0 && end > start, from);
    vm.runInContext(html.slice(start, end), ctx);
  }
  load('  function setSelectIfValid(', '  function renderSyncAndScopeMeta(');
  load('  var issuePresets =', '  function matrixButton(');
  load('  function resetBoardSelects(', '  function filterChip(');
  load('  document.getElementById("kpis").addEventListener', '  // Debounced');
  const crossBoardClick = html.indexOf('    var jumpPrBtn =');
  assert.notEqual(crossBoardClick, -1);
  const start = html.lastIndexOf('  document.addEventListener("click", function(e) {', crossBoardClick);
  const end = html.indexOf('\n  });', crossBoardClick);
  assert.ok(start >= 0 && end > start);
  vm.runInContext(html.slice(start, end + '\n  });'.length), ctx);
  ctx.initStateFromUrl();
  function click(selector, attrs, container) {
    const button = {
      dataset: attrs, getAttribute(name) { return attrs[name] ?? null; },
    };
    const event = { target: { closest(s) { return s === selector ? button : null; } } };
    if (container) el(container).handlers.click(event); else document.click(event);
  }
  return { ctx, el, click, document, location };
}

test('legacy matrix and stage deep links resolve to validated selects and canonical URLs', () => {
  const { ctx, el, location } = board('?view=prs&lang=en&cat=ENHANCEMENT&mdiff=HARD&msev=MAJOR&stage=CI_FAILED');
  assert.equal(el('catSel').value, 'HARD');
  assert.equal(el('sevSel').value, 'MAJOR');
  assert.equal(el('prStageSel').value, 'CI_FAILED');
  ctx.syncUrlState();
  assert.deepEqual(Object.fromEntries(location.searchParams), { view: 'prs', lang: 'en', cat: 'HARD', sev: 'MAJOR', stage: 'CI_FAILED' });
  const invalid = board('?cat=invalid&msev=invalid&mdiff=ENHANCEMENT&stage=bogus');
  invalid.ctx.syncUrlState();
  assert.equal(invalid.location.search, '');
});

test('matrix cells, row and column totals toggle ordinary filters and keep focus', () => {
  const { el, click, document, location } = board('?pr=open_fix');
  for (const [d, s, expected] of [
    ['EASY', 'MAJOR', ['EASY', 'MAJOR']], ['HARD', '', ['HARD', 'all']], ['', 'MINOR', ['REAL_ALL', 'MINOR']],
  ]) {
    const toggle = () => click('button[data-d][data-s]', { d, s }, 'mtxBody');
    toggle();
    assert.deepEqual([el('catSel').value, el('sevSel').value], expected);
    assert.equal(document.focused, '#mtxBody [data-d="' + d + '"][data-s="' + s + '"]');
    toggle();
    assert.equal(location.search, '?pr=open_fix');
  }
});

test('KPI presets replace conflicting severity and PR filters', () => {
  const { click, location } = board('?cat=HARD&pr=open_fix');
  click('[data-kpi-act]', { kpiAct: 'high' }, 'kpis');
  assert.equal(location.search, '?sev=HIGH_S0_S1');
  click('[data-kpi-act]', { kpiAct: 'pr_open' }, 'kpis');
  assert.equal(location.search, '?pr=open_fix');
  click('[data-kpi-act]', { kpiAct: 'all' }, 'kpis');
  assert.equal(location.search, '?cat=ALL');
});

test('cross-board jumps clear hiding filters while preserving sort and the source board', () => {
  const { click, el, location } = board('?cat=HARD&sort=updated_desc&pscope=real_bug_mainline&ball=AUTHOR&stage=CI_FAILED&psort=wait_desc');
  click('[data-jump-pr]', { 'data-jump-pr': '#12', 'data-pr-key': 'pingcap/tidb#12', 'data-is-cp': '1' });
  assert.equal(location.searchParams.get('view'), 'prs');
  assert.equal(el('prScopeSel').value, 'all_open');
  assert.equal(el('prStageSel').value, 'ALL');
  assert.equal(el('prBallSel').value, 'all');
  assert.equal(el('prSortSel').value, 'wait_desc');
  assert.equal(el('catSel').value, 'HARD');
  click('[data-jump-issue]', { 'data-jump-issue': '34' });
  assert.equal(el('q').value, '#34');
  assert.equal(el('catSel').value, 'ALL');
  assert.equal(el('sortSel').value, 'updated_desc');
  assert.equal(el('prQ').value, '#12');
});

test('removing a filter chip uses the same defaults as its board and clears a custom date', () => {
  const { click, el, location } = board('?scope=retriage&cat=HARD&win=custom&since=2026-10-01&psort=wait_desc');
  click('[data-clear-filter]', { 'data-clear-filter': 'cat' });
  assert.equal(el('catSel').value, 'REAL_ALL');
  click('[data-clear-filter]', { 'data-clear-filter': 'win' });
  assert.equal(el('sinceDate').value, '');
  assert.deepEqual(Object.fromEntries(location.searchParams), { psort: 'wait_desc', retriage: '1' });
});

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync(require('node:path').join(__dirname, 'index.html'), 'utf8');
function sourceFunction(name) {
  const start = html.indexOf('  function ' + name + '(');
  assert.notEqual(start, -1);
  // These top-level functions end at two-space indentation.
  const end = html.indexOf('\n  }', start);
  assert.notEqual(end, -1);
  return html.slice(start, end + 4);
}
function deferred() {
  let resolve;
  const promise = new Promise(r => { resolve = r; });
  return { promise, resolve };
}
test('old authentication failures cannot clear a newer session; current failures still do', async () => {
  const pending = deferred();
  const ctx = { workbenchRevision: 0, editingIssueNum: null, editDrawerGen: 0, sessionEpoch: 0, AUTH_REQUIRED: true, IS_AUTHENTICATED: true,
    fetch: () => pending.promise, appUrl: x => x,
    stripWorkbenchFromDataset: () => ctx.stripped++, freezeSessionDrafts: () => ctx.cleared++,
    applySessionState: () => ctx.applied++, openAuthModal: () => ctx.opened++,
    stripped: 0, cleared: 0, applied: 0, opened: 0 };
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('apiFetch'), ctx);
  const old = ctx.apiFetch('./api/state').catch(e => e);
  ctx.sessionEpoch++;
  pending.resolve({ ok: false, status: 401, text: async () => '{"error":"expired"}' });
  assert.equal((await old).status, 401);
  assert.deepEqual([ctx.sessionEpoch, ctx.stripped, ctx.cleared, ctx.applied, ctx.opened], [1, 0, 0, 0, 0]);
  ctx.fetch = async () => ({ ok: false, status: 401, text: async () => '{}' });
  await assert.rejects(ctx.apiFetch('./api/state'));
  assert.deepEqual([ctx.sessionEpoch, ctx.stripped, ctx.cleared, ctx.applied, ctx.opened], [2, 1, 1, 1, 1]);
  ctx.fetch = async () => ({ ok: true, text: async () => '{"ok":true}' });
  assert.equal((await ctx.apiFetch('./api/state')).ok, true);
});
test('pending review binding survives refresh and rebinds only on explicit review', () => {
  const ctx = { workbenchRevision: 0,};
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('copyReviewVersion') + '\n' + sourceFunction('pendingReviewBinding'), ctx);
  const first = { version: { review: 1, notes: 2 }, sync_watermark: 'old' };
  const newer = { version: { review: 2, notes: 3 }, sync_watermark: 'new' };
  const binding = ctx.pendingReviewBinding(null, first);
  first.version.review = 99;
  const preserved = ctx.pendingReviewBinding(binding, newer);
  assert.equal(preserved.version.review, 1);
  assert.equal(preserved.watermark, 'old');
  const reviewed = ctx.pendingReviewBinding(null, newer);
  assert.equal(reviewed.version.review, 2);
  assert.equal(reviewed.watermark, 'new');
  assert.equal(ctx.pendingReviewBinding(null, newer).version.notes, 3);
});
test('capturing form fields preserves the reviewed binding through a refresh', () => {
  const start = html.indexOf('    capturePendingCardDrafts = function(');
  const end = html.indexOf('\n    };\n\n    renderInboxDrawer', start);
  assert.ok(start >= 0 && end > start);
  const oldBinding = { version: { review: 4, notes: 5 }, watermark: 'before' };
  const fields = { '.p-cat': { value: 'HARD' }, '.p-note-en': { value: 'draft' } };
  const card = { getAttribute: () => '42', querySelector: selector => fields[selector] || null };
  const ctx = { workbenchRevision: 0, pendingDrafts: { '42': { binding: oldBinding } } };
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('capturePendingCardDraft') + html.slice(start, end + 7), ctx);
  ctx.capturePendingCardDrafts({ querySelectorAll: () => [card] });
  assert.equal(ctx.pendingDrafts['42'].binding, oldBinding);
  assert.equal(ctx.pendingDrafts['42'].cat, 'HARD');
  assert.equal(ctx.pendingDrafts['42'].noteEn, 'draft');
});

test('pending accept sends the version of the draft rather than the refreshed global candidate', () => {
  const marker = '    document.getElementById("inboxBody").addEventListener("click", function(e) {';
  const start = html.indexOf(marker);
  const end = html.indexOf('\n    });', start);
  assert.ok(start >= 0 && end > start);
  let click;
  let payload;
  const button = { disabled: false };
  const fields = {
    '.p-cat': { value: 'HARD' }, '.p-sev': { value: 'MAJOR' }, '.p-sub': { value: 'logical_rewrite' },
    '.p-bdr': { checked: false }, '.p-fk': { checked: false },
    '.p-note-zh': { value: '原始草稿根因' }, '.p-note-en': { value: 'Original review' },
  };
  const card = { getAttribute: () => '42', querySelector: selector => fields[selector] };
  const oldVersion = { review: 4, notes: 5 };
  const ctx = { workbenchRevision: 0,
    sessionEpoch: 1,
    document: { getElementById: () => ({ addEventListener: (_event, fn) => { click = fn; } }) },
    PENDING: [{ number: 42, version: { review: 9, notes: 5 }, sync_watermark: 'new' }],
    pendingDrafts: { '42': { binding: { version: oldVersion, watermark: 'old' } } },
    apiFetch: (_path, opts) => {
      payload = JSON.parse(opts.body);
      return { then: () => ({ catch: () => {} }) };
    },
  };
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('validReviewVersion') + html.slice(start, end + '\n    });'.length), ctx);
  click({ target: { closest: selector => selector === '[data-pend-num]' ? card : selector === '.p-accept-btn' ? button : null } });
  assert.deepEqual(payload.expected_version, oldVersion);
  assert.equal(payload.if_unmodified_since, 'old');
  assert.equal(payload.note.en, 'Original review');
  assert.equal(payload.cat, 'HARD');
});

test('first pending render prefills exclusion suggestion and preserves an intentionally empty draft', () => {
  const start = html.indexOf('      body.innerHTML = PENDING.map(function(pc) {');
  const end = html.indexOf('\n    }', start);
  assert.ok(start >= 0 && end > start);
  const ctx = { workbenchRevision: 0,
    body: {}, PENDING: [{ number: 42, suggested_triage: { exclude_reason: 'Executor issue' }, version: { review: 1, notes: 1 } }],
    pendingDrafts: {}, enums: { categories: [], severities: [], subsystems: [] },
    esc: v => String(v ?? ''), t: x => x, safeHref: x => x || '', fmtInlineCode: x => x || '',
    hasCanonicalLabels: () => false, ICONS: { check: '' },
  };
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('copyReviewVersion') + '\n' + sourceFunction('pendingReviewBinding'), ctx);
  const render = () => vm.runInContext(html.slice(start, end), ctx);
  render();
  assert.match(ctx.body.innerHTML, /class="p-ex-reason"[^>]*value="Executor issue"/);
  ctx.pendingDrafts['42'].exReason = '';
  render();
  assert.match(ctx.body.innerHTML, /class="p-ex-reason"[^>]*value=""/);
});

test('conflict refresh keeps pending binding until explicit review and preserves dirty fields', async () => {
  let rendered = false;
  const draft = { binding: { version: { review: 1, notes: 1 }, watermark: 'old' }, cat: 'HARD', sev: 'MAJOR', exReason: 'old suggestion', dirty: { cat: true } };
  const candidate = { number: 42, version: { review: 2, notes: 1 }, sync_watermark: 'new', suggested_triage: { severity: 'MINOR', exclude_reason: 'new suggestion' } };
  const ctx = { workbenchRevision: 0, sessionEpoch: 3, CAN_TRIAGE: true, pendingDrafts: { 42: draft },
    apiFetch: async () => ({ snapshot_revision: 1, dataset: {}, pending_candidates: [candidate], excluded_issues: [] }),
    applyWorkbenchSnapshot: resp => { ctx.PENDING = resp.pending_candidates; },
    document: { getElementById: () => ({ hidden: false }) },
    renderInboxDrawer: () => { rendered = true; }, refreshOpenEditDrawer: () => {} };
  vm.createContext(ctx);
  vm.runInContext(['copyReviewVersion', 'pendingReviewBinding', 'reviewLatestPending', 'refreshAfterConflict'].map(sourceFunction).join('\n'), ctx);
  await assert.rejects(ctx.refreshAfterConflict(new Error('conflict'), 3), /conflict/);
  assert.equal(rendered, true);
  assert.equal(draft.binding.version.review, 1);
  assert.equal(draft.sev, 'MAJOR');
  ctx.reviewLatestPending(draft, ctx.PENDING[0]);
  assert.equal(draft.binding.version.review, 2);
  assert.equal(draft.cat, 'HARD');
  assert.equal(draft.sev, undefined);
  assert.equal(draft.exReason, undefined);
});

test('snapshot replaces both queues and invalidates absent queues', () => {
  const ctx = { workbenchRevision: 0, document: { getElementById: () => ({}) }, capturePendingCardDrafts: () => {}, refreshOpenEditDrawer: () => {}, EXCLUDED_ITEMS: ['old'], PENDING: ['old'], setDatasetAndRender: (ds, p) => { ctx.dataset = ds; ctx.PENDING = p; } };
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('applyWorkbenchSnapshot'), ctx);
  const dataset = { id: 1 };
  ctx.applyWorkbenchSnapshot({ snapshot_revision: 1, dataset, pending_candidates: [1], excluded_issues: [2] });
  assert.equal(ctx.dataset, dataset);
  assert.equal(ctx.PENDING[0], 1);
  assert.equal(ctx.EXCLUDED_ITEMS[0], 2);
  ctx.applyWorkbenchSnapshot({ snapshot_revision: 1, dataset: { id: 2 } });
  assert.equal(ctx.PENDING.length, 0);
  assert.equal(ctx.EXCLUDED_ITEMS, null);
});

test('classification-only patch omits notes and empty acknowledgement is zero', () => {
  const ctx = { workbenchRevision: 0,};
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('noteForPatch') + '\n' + sourceFunction('acknowledgedCount'), ctx);
  assert.equal(ctx.noteForPatch({ 'zh-CN': 'note', en: '' }, { 'zh-CN': ' note ', en: '' }), undefined);
  assert.equal(ctx.noteForPatch({ 'zh-CN': 'changed', en: '' }, { 'zh-CN': 'note', en: '' })['zh-CN'], 'changed');
  assert.equal(ctx.acknowledgedCount({ acknowledged: [] }), 0);
  assert.equal(ctx.acknowledgedCount({}), 0);
});

test('passive auth loss freezes drafts; explicit logout clears them', () => {
  const { ctx, el } = editorHarness();
  ctx.openEditDrawer(42);
  el('edNoteEn').value = 'unsaved analysis';
  ctx.pendingDrafts[43] = { binding: { version: { review: 1, notes: 1 } } };
  ctx.freezeSessionDrafts();
  assert.equal(ctx.pendingDrafts[43].binding.version.review, 1);
  assert.equal(ctx.frozenEditDraft.values.edNoteEn, 'unsaved analysis');
  assert.equal(el('inboxBody').innerHTML, '');
  ctx.DATA[0].version = { review: 9, notes: 9 };
  ctx.restoreSessionDrafts();
  assert.equal(el('editDrawer').opener, el('[data-edit-issue="42"]'));
  assert.equal(ctx.editingDraft.version.review, 1);
  assert.equal(el('edNoteEn').value, 'unsaved analysis');
  assert.equal(el('editStaleNotice').hidden, false);
  ctx.clearSessionDrafts();
  assert.equal(Object.keys(ctx.pendingDrafts).length, 0);
  assert.equal(ctx.frozenEditDraft, null);
  assert.equal(ctx.editingDraft, null);
});

test('committed response refreshes state and reports saved without retrying the mutation', async () => {
  const calls = [];
  const el = { innerHTML: 'cards', hidden: false };
  const ctx = { workbenchRevision: 0, sessionEpoch: 1, CAN_TRIAGE: true, pendingDrafts: { 42: {}, 43: {} },
    document: { getElementById: () => el }, capturePendingCardDrafts: () => {}, closeEditDrawer: () => {},
    apiFetch: async path => { calls.push(path); return { snapshot_revision: 1, dataset: {}, pending_candidates: [], excluded_issues: [] }; },
    closeInboxDrawer() {}, syncOpenEditDrawerSessionUI() {}, renderAll() {},
    applyWorkbenchSnapshot: () => calls.push('applied'), renderInboxDrawer: () => {}, t: () => 'Saved, refresh failed' };
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('discardPendingDraft') + sourceFunction('handleCommittedWithoutSnapshot'), ctx);
  await assert.rejects(ctx.handleCommittedWithoutSnapshot('./api/pending/42/resolve', 1), e => e.committed && /Saved/.test(e.message));
  assert.deepEqual(calls, ['./api/state', 'applied']);
  assert.equal(ctx.pendingDrafts[42], undefined);
  assert.ok(ctx.pendingDrafts[43]);
});

test('editor re-review preserves only edited fields and binds all defaults to latest record', () => {
  const old = {number:42,cat:'MEDIUM',severity:'MAJOR',subsystem:'logical_rewrite',is_borderline_kept:false,force_keep:false,note:{en:'old note'},version:{review:1,notes:1}};
  const latest = {...old,cat:'HARD',severity:'CRITICAL',subsystem:'statistics',is_borderline_kept:true,force_keep:true,note:{en:'other note'},version:{review:2,notes:2}};
  const els = {};
  const ctx = { workbenchRevision: 0,DATA:[latest],CAN_TRIAGE:true,DATA_HAS_WORKBENCH:true,editingIssueNum:42,editingDraft:null,editDrawerGen:0,
    document:{getElementById:id=>els[id]},syncOpenEditDrawerSessionUI:()=>{}};
  vm.createContext(ctx);
  vm.runInContext(['recordNote','editValuesFromRecord','copyReviewVersion','validReviewVersion','editDraftFromRecord','canSubmitEditDraft','syncEditSeverityConstraints','reviewLatestEdit'].map(sourceFunction).join('\n'),ctx);
  ctx.editingDraft=ctx.editDraftFromRecord(old);
  for (const [id,value] of Object.entries(ctx.editingDraft.originalValues)) els[id]=typeof value==='boolean'?{type:'checkbox',checked:value}:{type:'text',value};
  els.edNoteEn.value='my draft';
  ctx.reviewLatestEdit();
  assert.equal(els.edCat.value,'HARD');
  assert.equal(els.edSev.value,'CRITICAL');
  assert.equal(els.edSub.value,'statistics');
  assert.equal(els.edBorderline.checked,true);
  assert.equal(els.edForceKeep.checked,true);
  assert.equal(els.edNoteEn.value,'my draft');
  assert.equal(ctx.editingDraft.version.review,2);
  assert.equal(ctx.editingDraft.originalNote.en,'other note');
  // Another re-review keeps an actual classification edit as well.
  els.edCat.value='EASY';
  ctx.DATA=[{...latest,cat:'MEDIUM',severity:'MINOR',version:{review:3,notes:2}}];
  ctx.reviewLatestEdit();
  assert.equal(els.edCat.value,'EASY');
  assert.equal(els.edSev.value,'MINOR');
});

test('delayed committed response cannot close a different or reopened editor', async () => {
  const ctx={sessionEpoch:1,CAN_TRIAGE:true,editingIssueNum:99,editDrawerGen:3,pendingDrafts:{},
    document:{getElementById:()=>({innerHTML:'',hidden:true})},capturePendingCardDrafts:()=>{},
    closeInboxDrawer(){},syncOpenEditDrawerSessionUI(){},renderAll(){},
    closeEditDrawer:()=>{ctx.closed++},closed:0,apiFetch:async()=>({}),applyWorkbenchSnapshot:()=>{},t:()=> 'Saved'};
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('discardPendingDraft') + sourceFunction('handleCommittedWithoutSnapshot'),ctx);
  await assert.rejects(ctx.handleCommittedWithoutSnapshot('./api/issues/42/triage',1,{number:42,generation:1}),e=>e.committed);
  assert.equal(ctx.closed,0);
  ctx.editingIssueNum=42;
  await assert.rejects(ctx.handleCommittedWithoutSnapshot('./api/issues/42/triage',1,{number:42,generation:1}),e=>e.committed);
  assert.equal(ctx.closed,0);
  await assert.rejects(ctx.handleCommittedWithoutSnapshot('./api/issues/42/triage',1,{number:42,generation:3}),e=>e.committed);
  assert.equal(ctx.closed,1);
});

// Exercise the actual capture/render/snapshot/event-handler chain with a small
// DOM adapter. Each mounted card owns its values, just like a replaced DOM node.
function inboxHarness() {
  let click, change;
  let cards = [];
  const body = {
    querySelectorAll: () => cards,
    addEventListener: (event, handler) => { if (event === 'click') click = handler; if (event === 'change') change = handler; },
    get innerHTML() { return this.html || ''; },
    set innerHTML(value) { this.html = value; cards = []; },
  };
  const ctx = { workbenchRevision: 0,
    sessionEpoch: 1, CAN_TRIAGE: true, PENDING: [], pendingDrafts: {}, EXCLUDED_ITEMS: [],
    document: { getElementById: id => id === 'inboxBody' ? body : {} },
    updateInboxTabsHeader: () => {}, activeInboxTab: 'pending', RETRIAGE_LIST: [],
    SiteI18n: { getLocale: () => 'en' }, esc: v => String(v ?? ''), t: x => x,
    safeHref: x => x || '', fmtInlineCode: x => x || '', hasCanonicalLabels: () => false,
    ICONS: { check: '' }, showToast: () => {}, refreshOpenEditDrawer: () => {},
    window: { REPORT_DATASET: { enums: { categories: ['EASY', 'HARD'], severities: ['MINOR', 'CRITICAL'], subsystems: ['logical_rewrite'] } } },
    setDatasetAndRender: (_ds, pending) => { ctx.PENDING = pending; },
  };
  vm.createContext(ctx);
  vm.runInContext(['focusAfterDrawerRender', 'copyReviewVersion', 'pendingReviewBinding', 'capturePendingCardDraft', 'discardPendingDraft',
    'applyWorkbenchSnapshot', 'reviewLatestPending', 'markPendingFieldDirty', 'validReviewVersion', 'cardReviewVersion'].map(sourceFunction).join('\n'), ctx);
  const start = html.indexOf('    capturePendingCardDrafts = function(');
  const end = html.indexOf('\n    document.getElementById("pendingInboxBtn")', start);
  vm.runInContext(html.slice(start, end), ctx);
  const changeStart = html.indexOf('    document.getElementById("inboxBody").addEventListener("change", function(e) {');
  const changeEnd = html.indexOf('\n    });', changeStart);
  vm.runInContext(html.slice(changeStart, changeEnd + '\n    });'.length), ctx);
  const clickStart = html.indexOf('    document.getElementById("inboxBody").addEventListener("click", function(e) {');
  const clickEnd = html.indexOf('\n    });', clickStart);
  vm.runInContext(html.slice(clickStart, clickEnd + '\n    });'.length), ctx);
  function mount(number) {
    const draft = ctx.pendingDrafts[number];
    const mapping = { cat: '.p-cat', sev: '.p-sev', sub: '.p-sub', bdr: '.p-bdr', fk: '.p-fk', noteZh: '.p-note-zh', noteEn: '.p-note-en', exReason: '.p-ex-reason' };
    const fields = {};
    const card = { getAttribute: () => String(number), querySelector: selector => fields[selector] };
    for (const [key, selector] of Object.entries(mapping)) fields[selector] = {
      value: draft[key], checked: draft[key], closest: () => card,
      classList: { contains: name => selector === '.' + name },
    };
    cards.push(card);
    return { card, fields };
  }
  function act(card, selector) {
    const button = { disabled: false };
    click({ target: { closest: s => s === '[data-pend-num]' ? card : s === selector ? button : null } });
  }
  return { ctx, body, mount, act, change: target => change({ target }) };
}

for (const action of ['accept', 'exclude']) {
  test(`successful pending ${action} discards its draft across requeue and preserves other drafts`, async () => {
    const { ctx, body, mount, act } = inboxHarness();
    const old = { number: 42, version: { review: 1, notes: 1 }, sync_watermark: 'old', suggested_triage: { cat: 'EASY', severity: 'MINOR' } };
    const other = { ...old, number: 43 };
    ctx.PENDING = [old, other];
    ctx.renderInboxDrawer();
    const first = mount(42);
    const second = mount(43);
    first.fields['.p-note-en'].value = 'old cause';
    second.fields['.p-note-en'].value = 'other unsaved cause';
    ctx.markPendingFieldDirty(second.fields['.p-note-en']);
    let submitted;
    ctx.apiFetch = async (_path, opts) => { submitted = JSON.parse(opts.body); return { snapshot_revision: 1, dataset: {}, pending_candidates: [other] }; };
    act(first.card, `.p-${action}-btn`);
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(submitted.action, action);
    assert.equal(ctx.pendingDrafts[42], undefined);
    assert.equal(ctx.pendingDrafts[43].noteEn, 'other unsaved cause');
    assert.equal(ctx.pendingDrafts[43].binding.version.review, 1);
    // A stale detached DOM node is never allowed to restore a consumed draft.
    ctx.capturePendingCardDraft(first.card);
    assert.equal(ctx.pendingDrafts[42], undefined);
    const latest = { ...old, version: { review: 10, notes: 3 }, sync_watermark: 'new', suggested_triage: { cat: 'HARD', severity: 'CRITICAL' } };
    ctx.applyWorkbenchSnapshot({ snapshot_revision: 1, dataset: {}, pending_candidates: [latest, other] });
    ctx.renderInboxDrawer();
    assert.match(body.innerHTML, /<option value="HARD" selected>/);
    const fresh = mount(42);
    assert.equal(fresh.fields['.p-note-en'].value, '');
    assert.equal(ctx.pendingDrafts[42].binding.version.review, 10);
    ctx.apiFetch = (_path, opts) => { submitted = JSON.parse(opts.body); return new Promise(() => {}); };
    act(fresh.card, '.p-accept-btn');
    assert.equal(submitted.cat, 'HARD');
    assert.equal(submitted.severity, 'CRITICAL');
    assert.equal(submitted.note.en, '');
    assert.equal(submitted.expected_version.review, 10);
  });
}

test('pending input survives tab and locale renders; refresh and explicit re-review have distinct ownership', () => {
  const { ctx, body, mount, act } = inboxHarness();
  const old = { number: 42, version: { review: 1, notes: 1 }, sync_watermark: 'old', suggested_triage: { cat: 'EASY', severity: 'MINOR' } };
  ctx.PENDING = [old];
  ctx.renderInboxDrawer();
  const { card, fields } = mount(42);
  fields['.p-note-en'].value = 'my analysis';
  ctx.markPendingFieldDirty(fields['.p-note-en']);
  ctx.activeInboxTab = 'retriage';
  ctx.renderInboxDrawer();
  ctx.activeInboxTab = 'pending';
  ctx.renderInboxDrawer();
  ctx.SiteI18n.getLocale = () => 'zh-CN';
  ctx.renderInboxDrawer();
  assert.equal(ctx.pendingDrafts[42].noteEn, 'my analysis');
  const latest = { ...old, version: { review: 10, notes: 1 }, sync_watermark: 'new', suggested_triage: { cat: 'HARD', severity: 'CRITICAL' } };
  ctx.applyWorkbenchSnapshot({ snapshot_revision: 1, dataset: {}, pending_candidates: [latest] });
  ctx.renderInboxDrawer();
  assert.equal(ctx.pendingDrafts[42].binding.version.review, 1);
  assert.equal(ctx.pendingDrafts[42].cat, 'EASY');
  assert.match(body.innerHTML, /p-review-btn/);
  // Mount the current rendered card. Explicit review must not recapture the
  // old defaults during its subsequent render.
  const refreshed = mount(42);
  act(refreshed.card, '.p-review-btn');
  assert.equal(ctx.pendingDrafts[42].binding.version.review, 10);
  assert.equal(ctx.pendingDrafts[42].cat, 'HARD');
  assert.equal(ctx.pendingDrafts[42].sev, 'CRITICAL');
  assert.equal(ctx.pendingDrafts[42].noteEn, 'my analysis');
});

function editorHarness() {
  const elements = {};
  function el(id) {
    return elements[id] || (elements[id] = {
      id, hidden: false, value: '', checked: false, disabled: false, open: false,
      type: ['edBorderline', 'edForceKeep'].includes(id) ? 'checkbox' : 'text',
      setAttribute() {}, handlers: {},
      addEventListener(event, handler) { this.handlers[event] = handler; },
    });
  }
  el('editDrawer').hidden = true;
  el('inboxDrawer').hidden = true;
  el('authModal').hidden = true;
  el('rubricDrawer').hidden = true;
  function record(number = 42, review = 1) {
    return { number, title: 'planner issue', author: 'dev', cat: 'MEDIUM', severity: 'MAJOR', subsystem: 'logical_rewrite',
      note: { en: 'cause', 'zh-CN': '根因' }, version: { review, notes: 1 } };
  }
  const ctx = { workbenchRevision: 0,
    DATA: [record()], CAN_TRIAGE: true, CAN_SYNC: true, DATA_HAS_WORKBENCH: true,
    editingIssueNum: null, editingDraft: null, editDrawerGen: 0, pendingDrafts: {}, frozenEditDraft: null,
    sessionEpoch: 1, AUTH_REQUIRED: true, IS_AUTHENTICATED: true,
    document: { getElementById: el, querySelector: el, handlers: {}, addEventListener(event, handler) { this.handlers[event] = handler; } },
    window: { REPORT_DATASET: { enums: { categories: [], severities: [], subsystems: [] } } },
    capturePendingCardDrafts() {}, populateSelectOptions() {}, renderDrawerRetriageBanner() {}, syncBodyDrawerLock() {},
    showDrawer(id, opener) {
      el(id).opener = opener;
      el(id).titleWhenOpened = el('editDrawerTitle').textContent;
      el(id).hidden = false;
    }, hideDrawer(id) { el(id).hidden = true; },
    SiteI18n: { getLocale: () => 'en' },
    applySessionChrome() {}, renderAll() {}, esc: x => x || '', t: (key, args) => key + (args ? JSON.stringify(args) : ''),
    fmtInlineCode: x => x || '', fmtUsers: x => x, formatPrChip: x => '', toasts: [], showToast: message => ctx.toasts.push(message),
    setDatasetAndRender: (ds, pending) => { ctx.DATA = ds.records; ctx.PENDING = pending; ctx.DATA_HAS_WORKBENCH = true; },
    calls: [], apiFetch: (path, opts) => { ctx.calls.push({ path, opts, payload: opts?.body ? JSON.parse(opts.body) : null }); return new Promise(() => {}); },
  };
  vm.createContext(ctx);
  const names = ['renderInspectSummary', 'activeDrawer', 'focusAfterDrawerRender', 'copyReviewVersion', 'recordNote', 'editValuesFromRecord', 'validReviewVersion', 'editDraftFromRecord',
    'canSubmitEditDraft', 'populateEditDrawerFormFromRecord', 'syncOpenEditDrawerSessionUI', 'openEditDrawer', 'closeEditDrawer',
    'closeInboxDrawer', 'readEditFormValues', 'restoreEditFormValues', 'freezeSessionDrafts', 'restoreSessionDrafts', 'clearSessionDrafts',
    'noteForPatch', 'submitCurrentOverride', 'syncEditSeverityConstraints', 'reviewLatestEdit', 'refreshOpenEditDrawer', 'applySessionState',
    'applyWorkbenchSnapshot', 'applyEditMutationSnapshot', 'submitAuthLogin', 'closeAuthModal'];
  vm.runInContext(names.map(sourceFunction).join('\n'), ctx);
  ctx.drawerClosers = { authModal: ctx.closeAuthModal, editDrawer: ctx.closeEditDrawer, inboxDrawer: ctx.closeInboxDrawer };
  for (const id of ['resetOverrideBtn', 'editRetriageBanner', 'edExcludeConfirmBtn']) {
    const start = html.indexOf(`    document.getElementById("${id}").addEventListener("click", function(`);
    const end = html.indexOf('\n    });', start);
    assert(start >= 0 && end > start);
    vm.runInContext(html.slice(start, end + '\n    });'.length), ctx);
  }
  const keyStart = html.indexOf('  document.addEventListener("keydown", function(e) {');
  const keyEnd = html.indexOf('\n  });', keyStart);
  vm.runInContext(html.slice(keyStart, keyEnd + '\n  });'.length), ctx);
  return { ctx, el, record };
}

test('login promotes an open Inspect using the new snapshot before keyboard submission', async () => {
  const { ctx, el, record } = editorHarness();
  ctx.CAN_TRIAGE = false;
  ctx.DATA_HAS_WORKBENCH = false;
  ctx.openEditDrawer(42);
  assert.match(el('editDrawer').titleWhenOpened, /inspectTitle.*42/);
  assert.equal(ctx.editingDraft, null);
  assert.equal(el('editFormWrap').hidden, true);
  assert.equal(el('editDrawerFtr').hidden, true);
  assert.match(el('inspectSummary').innerHTML, /report.enums.cat.MEDIUM/);
  assert.match(el('inspectSummary').innerHTML, /report.enums.subsystem.logical_rewrite/);
  assert.match(el('inspectSummary').innerHTML, /根因/);
  assert.match(el('inspectSummary').innerHTML, /cause/);
  el('authTokenInput').value = 'test-token';
  ctx.apiFetch = async path => path.endsWith('/login') ? {} : {
    session: { permissions: { can_triage: true, can_sync: true }, authenticated: true },
    snapshot_revision: 1, dataset: { records: [{ ...record(42, 9), cat: 'HARD' }] }, pending_candidates: [],
  };
  ctx.submitAuthLogin();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(ctx.editingDraft.version.review, 9);
  assert.equal(el('edCat').value, 'HARD');
  assert.equal(el('inspectSummary').hidden, true);
  assert.equal(el('editFormWrap').hidden, false);
  assert.equal(el('saveOverrideBtn').hidden, false);
  ctx.apiFetch = (path, opts) => { ctx.calls.push({ path, payload: JSON.parse(opts.body) }); return new Promise(() => {}); };
  ctx.document.handlers.keydown({ key: 'Enter', ctrlKey: true, preventDefault() {} });
  assert.equal(ctx.calls.length, 1);
  assert.equal(ctx.calls[0].payload.expected_version.review, 9);
  assert.equal(ctx.calls[0].payload.if_unmodified_since, undefined);
});

test('Escape closes only the top dialog and editor shortcuts stay inside the editor', () => {
  const { ctx, el } = editorHarness();
  el('inboxDrawer').hidden = false;
  ctx.openEditDrawer(42);
  el('authModal').hidden = false;
  const press = (key, ctrlKey = false) => ctx.document.handlers.keydown({ key, ctrlKey, preventDefault() {} });
  press('Enter', true);
  assert.equal(ctx.calls.length, 0);
  press('Escape');
  assert.equal(el('authModal').hidden, true);
  assert.equal(ctx.editingDraft.version.review, 1);
  press('Escape');
  assert.equal(el('editDrawer').hidden, true);
  assert.equal(el('inboxDrawer').hidden, false);
  press('Escape');
  assert.equal(el('inboxDrawer').hidden, true);
});

test('read-only Inspect cannot overwrite an earlier frozen writable draft', () => {
  const { ctx, el, record } = editorHarness();
  ctx.openEditDrawer(42);
  el('edNoteEn').value = 'my saved local analysis';
  ctx.freezeSessionDrafts();
  ctx.CAN_TRIAGE = false;
  ctx.DATA_HAS_WORKBENCH = false;
  ctx.DATA.push(record(43));
  ctx.openEditDrawer(43);
  ctx.freezeSessionDrafts();
  assert.equal(ctx.frozenEditDraft.num, 42);
  assert.equal(ctx.frozenEditDraft.binding.version.review, 1);
  ctx.CAN_TRIAGE = true;
  ctx.DATA_HAS_WORKBENCH = true;
  ctx.DATA[0] = record(42, 10);
  ctx.restoreSessionDrafts();
  assert.equal(ctx.editingIssueNum, 42);
  assert.equal(ctx.editingDraft.version.review, 1);
  assert.equal(el('edNoteEn').value, 'my saved local analysis');
  assert.equal(el('editStaleNotice').hidden, false);
});

test('snapshot reconciles editor metadata and stale notice without replacing its reviewed draft', () => {
  const { ctx, el, record } = editorHarness();
  ctx.openEditDrawer(42);
  el('edNoteEn').value = 'local changes';
  el('editExcludeZone').open = true;
  el('edExcludeReason').value = 'draft reason';
  ctx.applyWorkbenchSnapshot({ snapshot_revision: 1, dataset: { records: [{ ...record(42, 8), title: 'updated title' }] }, pending_candidates: [] });
  assert.equal(ctx.editingDraft.version.review, 1);
  assert.equal(el('edNoteEn').value, 'local changes');
  assert.equal(el('edExcludeReason').value, 'draft reason');
  assert.equal(el('editExcludeZone').open, true);
  assert.match(el('editIssueHeading').innerHTML, /updated title/);
  assert.equal(el('editStaleNotice').hidden, false);
});

test('snapshot closes an editor whose issue left the active list', () => {
  const { ctx, el } = editorHarness();
  ctx.openEditDrawer(42);
  ctx.applyWorkbenchSnapshot({ snapshot_revision: 1, dataset: { records: [] }, pending_candidates: [] });
  assert.equal(ctx.editingIssueNum, null);
  assert.equal(ctx.editingDraft, null);
  assert.equal(el('editDrawer').hidden, true);
  assert.match(ctx.toasts.at(-1), /issueUnavailable/);
  ctx.submitCurrentOverride();
  assert.equal(ctx.calls.length, 0);
});

test('all editor mutations reject missing or malformed review bindings', () => {
  for (const version of [null, {}, { review: 1 }, { review: 0, notes: 1 }, { review: 1.5, notes: 1 }, { review: 1, notes: -1 }]) {
    const { ctx, el } = editorHarness();
    ctx.openEditDrawer(42);
    ctx.editingDraft.version = version;
    ctx.submitCurrentOverride();
    el('resetOverrideBtn').handlers.click.call(el('resetOverrideBtn'));
    el('edExcludeConfirmBtn').handlers.click.call(el('edExcludeConfirmBtn'));
    el('editRetriageBanner').handlers.click({ target: { closest: () => ({ disabled: false }) } });
    assert.equal(ctx.calls.length, 0, JSON.stringify(version));
  }
});

test('re-review normalizes category and severity together before submitting', () => {
  const { ctx, el, record } = editorHarness();
  ctx.openEditDrawer(42);
  el('edCat').value = 'EASY';
  ctx.applyWorkbenchSnapshot({ snapshot_revision: 1, dataset: { records: [{ ...record(42, 9), cat: 'ENHANCEMENT', severity: 'N/A' }] } });
  ctx.reviewLatestEdit();
  assert.equal(el('edCat').value, 'EASY');
  assert.equal(el('edSev').value, 'MODERATE');
  ctx.submitCurrentOverride();
  assert.equal(ctx.calls[0].payload.severity, 'MODERATE');
  assert.equal(ctx.calls[0].payload.expected_version.review, 9);
});

test('successful exclusion closes its own editor without reporting a concurrent disappearance', async () => {
  const { ctx, el } = editorHarness();
  ctx.openEditDrawer(42);
  ctx.apiFetch = async () => ({ snapshot_revision: 1, dataset: { records: [] }, pending_candidates: [] });
  el('edExcludeConfirmBtn').handlers.click.call(el('edExcludeConfirmBtn'));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(ctx.editingIssueNum, null);
  assert.equal(el('editDrawer').hidden, true);
  assert.equal(ctx.toasts.some(message => message.includes('issueUnavailable')), false);
  assert.match(ctx.toasts.at(-1), /toastIssueExcluded/);
});

test('a delayed editor response reconciles but does not close a different editor', async () => {
  const { ctx, el, record } = editorHarness();
  const response = deferred();
  ctx.DATA.push(record(43));
  ctx.openEditDrawer(42);
  ctx.apiFetch = () => response.promise;
  ctx.submitCurrentOverride();
  ctx.openEditDrawer(43);
  el('edNoteEn').value = 'other draft';
  response.resolve({ snapshot_revision: 1, dataset: { records: [record(42, 2), record(43, 3)] }, pending_candidates: [] });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(ctx.editingIssueNum, 43);
  assert.equal(ctx.editingDraft.version.review, 1);
  assert.equal(el('edNoteEn').value, 'other draft');
  assert.equal(el('editStaleNotice').hidden, false);
});

test('header sync and conflict refresh share the editor snapshot reconciliation', async () => {
  const { ctx, el, record } = editorHarness();
  ctx.openEditDrawer(42);
  el('edNoteEn').value = 'local note';
  const start = html.indexOf('    function bindHeaderSyncAction(');
  const end = html.indexOf('\n    bindHeaderSyncAction("syncPrsBtn"', start);
  assert(start >= 0 && end > start);
  vm.runInContext(html.slice(start, end) + sourceFunction('refreshAfterConflict'), ctx);
  ctx.bindHeaderSyncAction('syncPrsBtn', 'syncPrsBtnLbl', 'idle', './api/sync/prs', () => {});
  ctx.apiFetch = async () => ({ snapshot_revision: 1, dataset: { records: [record(42, 9)] }, pending_candidates: [] });
  el('syncPrsBtn').handlers.click.call(el('syncPrsBtn'));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(el('editStaleNotice').hidden, false);
  assert.equal(ctx.editingDraft.version.review, 1);
  assert.equal(el('edNoteEn').value, 'local note');
  ctx.apiFetch = async () => ({ snapshot_revision: 1, dataset: { records: [] }, pending_candidates: [] });
  await assert.rejects(ctx.refreshAfterConflict(new Error('conflict'), 1), /conflict/);
  assert.equal(el('editDrawer').hidden, true);
  assert.match(ctx.toasts.at(-1), /issueUnavailable/);
});

for (const committedError of [false, true]) {
  test(`a delayed ${committedError ? 'committed error' : 'save'} preserves an explicitly re-reviewed draft`, async () => {
    const { ctx, el, record } = editorHarness();
    const response = deferred();
    const snapshot = { snapshot_revision: 1, dataset: { records: [{ ...record(42, 2), note: { en: 'first save', 'zh-CN': '根因' } }] }, pending_candidates: [] };
    ctx.appUrl = x => x;
    ctx.AUTH_REQUIRED = false;
    ctx.renderInboxDrawer = () => {};
    ctx.fetch = path => path.endsWith('/state')
      ? Promise.resolve({ ok: true, text: async () => JSON.stringify(snapshot) }) : response.promise;
    vm.runInContext(sourceFunction('apiFetch') + sourceFunction('handleCommittedWithoutSnapshot'), ctx);
    ctx.openEditDrawer(42);
    el('edNoteEn').value = 'first save';
    ctx.submitCurrentOverride();
    // Sync observes the committed edit before the save response arrives.
    ctx.applyWorkbenchSnapshot(snapshot);
    ctx.reviewLatestEdit();
    el('edNoteEn').value = 'second analysis not submitted';
    response.resolve({ ok: !committedError, status: committedError ? 500 : 200,
      text: async () => JSON.stringify(committedError ? { committed: true, error: 'snapshot failed' } : snapshot) });
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(el('editDrawer').hidden, false);
    assert.equal(ctx.editingIssueNum, 42);
    assert.equal(ctx.editingDraft.version.review, 2);
    assert.equal(el('edNoteEn').value, 'second analysis not submitted');
  });
}


test('inbox acknowledgement and restore require valid rendered review versions', () => {
  const { ctx } = inboxHarness();
  const calls = [], toasts = [];
  ctx.apiFetch = (path, opts) => { calls.push({ path, body: JSON.parse(opts.body) }); return new Promise(() => {}); };
  ctx.showToast = message => toasts.push(message);
  let cards = [];
  ctx.document.querySelectorAll = () => cards;
  const start = html.indexOf('    document.getElementById("inboxBody").addEventListener("click", function(e) {');
  const end = html.indexOf('\n    });', start);
  let click;
  ctx.document.getElementById = () => ({ addEventListener: (_, fn) => { click = fn; } });
  vm.runInContext(html.slice(start, end + '\n    });'.length), ctx);
  function card(num, version) {
    return { getAttribute: name => name === 'data-review-version' ? version : name === 'data-review-watermark' ? 'legacy' : String(num) };
  }
  function act(selector, current) {
    const button = { disabled: false };
    click({ target: { closest: s => s === selector ? button : (s === '[data-retriage-num]' && selector === '.r-keep-btn') || (s === '[data-ex-num]' && selector === '.x-restore-btn') ? current : null } });
    return button;
  }
  for (const value of [null, 'null', '{}', '{', '{"review":1}', '{"review":0,"notes":1}', '{"review":1.5,"notes":1}', '{"review":1,"notes":-1}']) {
    const invalid = card(42, value);
    for (const selector of ['.r-keep-btn', '.x-restore-btn']) {
      assert.equal(act(selector, invalid).disabled, false, `${selector}: ${value}`);
    }
    cards = [card(41, '{"review":1,"notes":2}'), invalid];
    assert.equal(act('#inboxKeepAllBtn').disabled, false, `batch: ${value}`);
  }
  assert.equal(calls.length, 0);
  assert.equal(toasts.length, 24);
  assert.ok(toasts.every(message => message === 'report.rw.inbox.missingReviewVersion'));
  const valid = card(42, '{"review":3,"notes":4}');
  act('.r-keep-btn', valid);
  act('.x-restore-btn', valid);
  cards = [card(41, '{"review":1,"notes":2}'), valid];
  act('#inboxKeepAllBtn');
  assert.equal(calls.length, 3);
  assert.deepEqual(calls[0].body, { expected_version: { review: 3, notes: 4 } });
  assert.deepEqual(calls[1].body, { expected_version: { review: 3, notes: 4 } });
  assert.deepEqual(calls[2].body, { numbers: [41, 42], expected_versions: { 41: { review: 1, notes: 2 }, 42: { review: 3, notes: 4 } } });
});

test('category edits retain only coupled severity changes during review latest', () => {
  for (const [category, initialSev, expectedSev, dirtySev] of [
    ['HARD', 'MINOR', 'CRITICAL', false],
    ['ENHANCEMENT', 'MINOR', 'N/A', true],
    ['HARD', 'N/A', 'MAJOR', true],
  ]) {
    const { ctx, mount, act, change } = inboxHarness();
    const old = { number: 42, version: { review: 1, notes: 1 }, suggested_triage: { cat: initialSev === 'N/A' ? 'ENHANCEMENT' : 'EASY', severity: initialSev } };
    ctx.PENDING = [old];
    ctx.renderInboxDrawer();
    const { card, fields } = mount(42);
    fields['.p-cat'].value = category;
    change(fields['.p-cat']);
    assert.equal(Boolean(ctx.pendingDrafts[42].dirty.sev), dirtySev);
    ctx.PENDING = [{ ...old, version: { review: 2, notes: 1 }, suggested_triage: { cat: 'HARD', severity: 'CRITICAL' } }];
    act(card, '.p-review-btn');
    assert.equal(ctx.pendingDrafts[42].cat, category);
    assert.equal(ctx.pendingDrafts[42].sev, expectedSev);
    assert.equal(ctx.pendingDrafts[42].binding.version.review, 2);
  }
});

test('review latest normalizes suggested severity against retained category', () => {
  const { ctx, mount, act } = inboxHarness();
  ctx.PENDING = [{ number: 42, version: { review: 1, notes: 1 }, suggested_triage: { cat: 'EASY', severity: 'MINOR' } }];
  ctx.renderInboxDrawer();
  const { card, fields } = mount(42);
  fields['.p-cat'].value = 'HARD';
  ctx.markPendingFieldDirty(fields['.p-cat']);
  ctx.PENDING = [{ number: 42, version: { review: 2, notes: 1 }, suggested_triage: { cat: 'ENHANCEMENT', severity: 'N/A' } }];
  act(card, '.p-review-btn');
  assert.equal(ctx.pendingDrafts[42].cat, 'HARD');
  assert.equal(ctx.pendingDrafts[42].sev, 'MAJOR');
});

test('a delayed older mutation cannot remove a newly accepted issue or its draft', async () => {
  const { ctx, el, record } = editorHarness();
  const delayed = deferred();
  ctx.openEditDrawer(42);
  ctx.apiFetch = () => delayed.promise;
  ctx.submitCurrentOverride();
  ctx.closeEditDrawer();
  const older = { snapshot_revision: 10, dataset: { records: [record(42, 2)] }, pending_candidates: [{ number: 43 }] };
  ctx.applyWorkbenchSnapshot({ snapshot_revision: 11, dataset: { records: [record(42, 2), record(43, 2)] }, pending_candidates: [], excluded_issues: [] });
  ctx.openEditDrawer(43);
  el('edNoteEn').value = 'new unsaved analysis';
  delayed.resolve(older);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(ctx.editingIssueNum, 43);
  assert.equal(ctx.editingDraft.version.review, 2);
  assert.equal(el('editDrawer').hidden, false);
  assert.equal(el('edNoteEn').value, 'new unsaved analysis');
  assert.equal(ctx.DATA.length, 2);
  assert.equal(ctx.PENDING.length, 0);
  assert.equal(ctx.workbenchRevision, 11);
  assert.equal(ctx.toasts.some(message => message.includes('issueUnavailable')), false);
  // A real later removal must still reconcile and close the editor.
  ctx.applyWorkbenchSnapshot({ ...older, snapshot_revision: 12 });
  assert.equal(el('editDrawer').hidden, true);
});

test('snapshot ordering rejects malformed revisions and survives an auth transition', () => {
  const { ctx, record } = editorHarness();
  ctx.applyWorkbenchSnapshot({ snapshot_revision: 20, dataset: { records: [record(42)] }, pending_candidates: [], excluded_issues: [] });
  ctx.sessionEpoch += 1;
  for (const revision of [undefined, null, 0, -1, 1.5, '21', 19]) {
    assert.equal(ctx.applyWorkbenchSnapshot({ snapshot_revision: revision, dataset: { records: [] } }), false);
    assert.equal(ctx.DATA.length, 1);
    assert.equal(ctx.workbenchRevision, 20);
  }
  assert.equal(ctx.applyWorkbenchSnapshot({ snapshot_revision: 21, dataset: { records: [] }, pending_candidates: [], excluded_issues: [] }), true);
  assert.equal(ctx.DATA.length, 0);
});

for (const freshRevision of [null, 10, 20]) {
  test(`failed committed recovery preserves the last successful snapshot (${freshRevision})`, async () => {
    const { ctx, el, record } = editorHarness();
    let rejectRecovery;
    const recovery = new Promise((_resolve, reject) => { rejectRecovery = reject; });
    ctx.appUrl = x => x;
    ctx.AUTH_REQUIRED = false;
    ctx.renderInboxDrawer = () => {};
    ctx.fetch = path => path.endsWith('/state') ? recovery : Promise.resolve({
      ok: false, status: 500, text: async () => JSON.stringify({ committed: true, error: 'snapshot failed' })
    });
    vm.runInContext(sourceFunction('apiFetch') + sourceFunction('discardPendingDraft') + sourceFunction('handleCommittedWithoutSnapshot'), ctx);
    const snapshot = { snapshot_revision: 10, dataset: { records: [record(42), record(43)] },
      pending_candidates: [{ number: 44 }], excluded_issues: [{ number: 45 }] };
    ctx.applyWorkbenchSnapshot(snapshot);
    ctx.openEditDrawer(42);
    ctx.submitCurrentOverride();
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(ctx.DATA_HAS_WORKBENCH, false);
    if (freshRevision !== null) ctx.applyWorkbenchSnapshot({ ...snapshot, snapshot_revision: freshRevision });
    ctx.openEditDrawer(43);
    el('edNoteEn').value = 'new unsaved analysis';
    rejectRecovery(new Error('temporary connection failure'));
    await new Promise(resolve => setImmediate(resolve));
    const recovered = freshRevision !== null;
    assert.equal(ctx.DATA_HAS_WORKBENCH, recovered);
    assert.equal(ctx.PENDING.length, recovered ? 1 : 0);
    assert.equal(ctx.EXCLUDED_ITEMS?.length ?? 0, recovered ? 1 : 0);
    assert.equal(ctx.canSubmitEditDraft(), recovered);
    assert.equal(el('edNoteEn').disabled, !recovered);
    assert.equal(el('saveOverrideBtn').hidden, !recovered);
    assert.equal(el('edNoteEn').value, 'new unsaved analysis');
    assert.equal(ctx.workbenchRevision, freshRevision ?? 10);
  });
}

test('GitHub session expiry preserves login provider without navigating away from drafts', async () => {
  let next;
  const ctx = {
    SESSION: { github_login: true, token_login: false },
    workbenchRevision: 0, editingIssueNum: null, editDrawerGen: 0, sessionEpoch: 1,
    AUTH_REQUIRED: true, IS_AUTHENTICATED: true,
    fetch: async () => ({ ok: false, status: 403, text: async () => '{"error":"triage permission required"}' }),
    appUrl: x => x, freezeSessionDrafts() {}, stripWorkbenchFromDataset() {},
    applySessionState: value => { next = value; },
    openAuthModal: () => assert.fail('must not discard drafts by redirecting automatically'),
  };
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('apiFetch'), ctx);
  await assert.rejects(ctx.apiFetch('./api/triage/decisions', { method: 'POST' }));
  assert.equal(next.github_login, true);
  assert.equal(next.token_login, false);
  assert.equal(next.authenticated, false);
});

test('GitHub sign-in and identity display follow session permissions', () => {
  const elements = {};
  for (const id of ['permBadge', 'rwBar', 'pendingInboxBtn', 'syncPrsBtn', 'syncIssuesBtn',
    'authUnlockBtn', 'authLogoutBtn', 'githubSignInBtn', 'githubIdentity', 'pendingCntBadge']) {
    elements[id] = { hidden: false, textContent: '', classList: { toggle() {} } };
  }
  const ctx = {
    SESSION: { github_login: true, viewer: { login: 'someone' } },
    CAN_TRIAGE: false, CAN_SYNC: false, AUTH_REQUIRED: true, IS_AUTHENTICATED: false,
    PENDING: [], RETRIAGE_LIST: [], t: x => x,
    document: { getElementById: id => elements[id] || null },
  };
  vm.createContext(ctx);
  vm.runInContext(sourceFunction('applySessionChrome'), ctx);
  ctx.applySessionChrome();
  assert.equal(elements.githubSignInBtn.hidden, false);
  assert.equal(elements.authUnlockBtn.hidden, true);
  ctx.IS_AUTHENTICATED = true;
  ctx.applySessionChrome();
  assert.equal(elements.githubSignInBtn.hidden, true);
  assert.equal(elements.githubIdentity.textContent, '@someone');
  assert.equal(elements.authLogoutBtn.hidden, false);
  assert.equal(elements.syncPrsBtn.hidden, true);
});

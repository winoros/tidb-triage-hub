const { test } = require('node:test');
const assert = require('node:assert/strict');
const { openReport } = require('./report-fixture.cjs');

const pr = { repo: 'pingcap/tidb', num: 20, label: '#20', title: 'Fix',
  state: 'OPEN', url: 'https://github.com/pingcap/tidb/pull/20' };
const dataset = {
  sync_meta: { snapshot_utc: '2026-11-01T00:00:00Z' },
  enums: { categories: ['MEDIUM'], severities: ['MAJOR'], subsystems: ['statistics'] },
  records: [10, 11].map(number => ({ number, title: 'Issue ' + number, cat: 'MEDIUM',
    severity: 'MAJOR', gh_severity: 'major', needs_retriage: true, prs: [pr] })),
  pr_status: { 'pingcap/tidb#20': { ...pr, blocker_stage: 'CI_FAILED', ball_owner: 'AUTHOR',
    failed_checks: ['build', 'unit', 'lint', 'integration'].map(name => ({ name })) } },
};

test('sorting preserves expanded and collapsed CI checks', async t => {
  const page = await openReport(t, dataset, 'view=prs');
  const row = page.locator('[data-pr-row="pingcap/tidb#20"]');
  const summary = row.locator('summary');
  await summary.click();
  await page.locator('#prSortSel').selectOption('wait_desc');
  assert.equal(await row.locator('.ci-fail-chip:visible').count(), 4);
  await summary.click();
  await page.locator('#prSortSel').selectOption('num_desc');
  assert.equal(await row.locator('.ci-fail-chip:visible').count(), 3);
});

test('inbox redraw restores the same control in the same card', async t => {
  const page = await openReport(t, dataset, 'lang=en', [{ number: 30, title: 'Pending issue' }]);
  await page.locator('#pendingInboxBtn').click();
  // A locale change uses the same inbox redraw as a workbench snapshot.
  async function checkFocus(control, locale) {
    await control.focus();
    await page.evaluate(lang => SiteI18n.setLocale(lang, { persist: false, syncUrl: false }), locale);
    assert.ok(await control.evaluate(el => el === document.activeElement));
  }
  await checkFocus(page.locator('[data-pend-num="30"] summary'), 'zh-CN');
  await page.locator('#inboxTabRetriageBtn').click();
  const card = page.locator('[data-retriage-num="11"]');
  await checkFocus(card.locator('.pr-row a'), 'en');
  await checkFocus(card.locator('[data-pr-key="pingcap/tidb#20"]'), 'zh-CN');
});

test('issue overview plots global observations by calendar day', async t => {
  const history = [
    { date: '2026-11-01', count: 3 }, { date: '2026-11-02', count: 0 }, { date: '2026-11-05', count: 2 },
  ];
  const page = await openReport(t, { ...dataset, bug_history: history }, 'lang=en');
  const plot = page.locator('#bugTrendPlot');
  assert.ok(await plot.isVisible());
  const positions = await plot.evaluate(canvas => {
    const points = Chart.getChart(canvas).getDatasetMeta(0).data;
    const bounds = canvas.getBoundingClientRect();
    return points.map(p => ({ x: bounds.left + p.x, y: bounds.top + p.y }));
  });
  const [a, b, c] = positions;
  assert.ok(Math.abs((b.x - a.x) / (c.x - a.x) - 0.25) < 0.001);
  await page.mouse.move(b.x, b.y);
  await page.waitForFunction(() => Chart.getChart('bugTrendPlot').tooltip.title?.[0] === '2026-11-02');
  assert.equal(await plot.evaluate(canvas => Chart.getChart(canvas).tooltip.body[0].lines[0]), '0');
  await page.mouse.down();
  await page.mouse.move(c.x - 1, c.y, { steps: 5 });
  await page.mouse.up();
  await page.locator('#trendReset').click();
  assert.ok(await page.locator('#trendReset').isHidden());
  await page.locator('#q').fill('no matches');
  assert.equal(await page.locator('#bugTrendLatest').textContent(), '2026-11-05 · 2');
});

test('trend controls and canvas stay in place throughout a drag zoom and reset', async t => {
  const history = [290, 280, 285, 300, 309].map((count, i) => ({ date: '2026-11-2' + i, count }));
  const page = await openReport(t, { ...dataset, bug_history: history }, 'lang=zh-CN');
  const canvas = page.locator('#bugTrendPlot');
  const reset = page.locator('#trendReset');
  const layout = () => canvas.evaluate(el => {
    const card = el.closest('figure');
    const top = card.getBoundingClientRect().top;
    return [card, card.querySelector('figcaption'), card.querySelector('.trend-readout'),
      card.querySelector('#trendReset'), el].map(node => {
      const r = node.getBoundingClientRect();
      return [r.left, r.top - top, r.width, r.height];
    });
  });
  for (const width of [1280, 320]) {
    await page.setViewportSize({ width, height: 900 });
    await canvas.scrollIntoViewIfNeeded();
    await page.waitForFunction(() => Chart.getChart('bugTrendPlot').width ===
      Math.round(document.querySelector('.trend-chart').getBoundingClientRect().width));
    const before = await layout();
    const bounds = await canvas.boundingBox();
    const y = bounds.y + bounds.height / 2;
    await page.mouse.move(bounds.x + bounds.width / 3, y);
    await page.mouse.down();
    await page.mouse.move(bounds.x + bounds.width * 0.8, y, { steps: 5 });
    assert.deepEqual(await layout(), before);
    await page.mouse.up();
    await reset.waitFor({ state: 'visible' });
    assert.deepEqual(await layout(), before);
    await reset.click();
    assert.deepEqual(await layout(), before);
  }
});

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

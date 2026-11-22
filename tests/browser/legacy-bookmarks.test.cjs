const { test } = require('node:test');
const assert = require('node:assert/strict');
const { openReport } = require('./report-fixture.cjs');

function issue(number, cat, severity, gh_severity, flags = {}) {
  return { number, title: 'Issue ' + number, cat, severity, gh_severity, prs: [], ...flags };
}

const dataset = {
  sync_meta: { snapshot_utc: '2026-11-01T00:00:00Z' },
  records: [
    issue(101, 'HARD', 'MAJOR', 'unlabeled', { is_customer: true, needs_retriage: true }),
    issue(102, 'EASY', 'MAJOR', 'major', { needs_retriage: true }),
    issue(103, 'HARD', 'MINOR', 'unlabeled', { is_customer: true }),
    issue(104, 'ENHANCEMENT', 'N/A', 'unlabeled', { is_customer: true, needs_retriage: true }),
  ],
};

for (const [query, expected, canonical] of [
  ['msev=MAJOR&sev=GH_UNLABELED', [101], { sev: 'MAJOR', unlabeled: '1' }],
  ['msev=MAJOR&sev=MINOR', [], { sev: 'NONE' }],
  ['msev=MAJOR&sev=HIGH_S0_S1', [102, 101]],
  ['msev=MINOR&sev=HIGH_S0_S1', []],
  ['msev=MAJOR', [102, 101], { sev: 'MAJOR' }],
  ['sev=GH_UNLABELED', [103, 101]],
  ['scope=customer&cat=RETRIAGE', [104, 101], { scope: 'customer', cat: 'ALL', retriage: '1' }],
  ['cat=RETRIAGE', [104, 102, 101], { cat: 'ALL', retriage: '1' }],
  ['scope=customer', [103, 101]],
  ['scope=retriage', [104, 102, 101]],
  ['scope=retriage&cat=REAL_ALL', [102, 101]],
  ['cat=RETRIAGE&msev=MINOR', [103]],
  ['cat=ENHANCEMENT&mdiff=HARD&msev=MAJOR', [101], { cat: 'HARD', sev: 'MAJOR' }],
]) {
  test(query, async t => {
    const page = await openReport(t, dataset, query);
    async function assertIssueRows() {
      const links = await page.locator('#tbody .id-num-link:visible').allTextContents();
      assert.deepEqual(links, expected.map(number => '#' + number));
      if (!expected.length) assert.ok(await page.locator('#tbody .empty-state').isVisible());
    }

    await assertIssueRows();
    const normalized = page.url();
    if (canonical) assert.deepEqual(Object.fromEntries(new URL(normalized).searchParams), canonical);
    await page.reload();
    await assertIssueRows();
    assert.equal(page.url(), normalized);
  });
}

test('re-triage narrows the selected category and scope independently', async t => {
  const page = await openReport(t, dataset, 'scope=customer');
  await page.locator('[aria-controls="issueAdvanced"]').click();
  await page.locator('#retriage').check();
  const rows = page.locator('#tbody .id-num-link:visible');
  assert.deepEqual(await rows.allTextContents(), ['#101']);
  await page.locator('#catSel').selectOption('ALL');
  assert.deepEqual(await rows.allTextContents(), ['#104', '#101']);
  await page.locator('[data-clear-filter="retriage"]').click();
  assert.deepEqual(await rows.allTextContents(), ['#104', '#103', '#101']);
  assert.equal(await page.locator('#scopeSel').inputValue(), 'customer');
});

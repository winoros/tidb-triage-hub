const { before, after } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const { chromium } = require('playwright');

const web = join(__dirname, '../../web');
const template = readFileSync(join(web, 'index.html'), 'utf8');
const i18n = ['i18n.js', 'zh-CN.js', 'en.js']
  .map(name => readFileSync(join(web, 'i18n', name), 'utf8')).join('\n');

let browser;
before(async () => { browser = await chromium.launch(); });
after(async () => { await browser?.close(); });

async function openReport(t, dataset, query, pending = []) {
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(async () => { await page.close(); assert.deepEqual(errors, []); });
  const boot = 'window.REPORT_DATASET = ' + JSON.stringify(dataset) +
    '; window.TRIAGE_PENDING = ' + JSON.stringify(pending) +
    '; window.TRIAGE_SESSION = {permissions: {can_triage: true}};';
  const html = template.replace('<!--SERVER_INLINE_BUNDLE-->', () => '<script>' + i18n + '\n' + boot + '</script>');
  await page.route('**/*', route => route.request().isNavigationRequest()
    ? route.fulfill({ contentType: 'text/html', body: html })
    : route.abort());
  await page.goto('https://triage.test/?' + query);
  return page;
}

module.exports = { openReport };

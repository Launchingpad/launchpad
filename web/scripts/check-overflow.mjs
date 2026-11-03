// Phone-width overflow audit. Needs the dev server on :5173 (VITE_DEV_TOKEN, signed in)
// and `vite preview` on :4173 (login). Exits 1 if any page has an element outside the viewport.
import puppeteer from 'puppeteer-core';
import { auditOverflow } from './lib/audit.mjs';

const APP = process.env.APP ?? 'http://localhost:5173';
const LOGIN_APP = process.env.LOGIN_APP ?? 'http://localhost:4173';
const browser = await puppeteer.launch({
  executablePath: process.env.CHROME ?? '/Applications/Brave Browser.app/Contents/MacOS/Brave Browser',
  headless: true,
  userDataDir: `${process.env.TMPDIR}/chrome-profile-overflow`,
  args: ['--no-first-run', '--no-default-browser-check'],
});
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
for (const width of [375, 320]) {
  const run = async (label, fn, base = APP) => {
    const p = await browser.newPage();
    p.setDefaultTimeout(60000);
    await p.setViewport({ width, height: 812, deviceScaleFactor: 1, isMobile: true, hasTouch: true });
    await fn(p, base);
    await sleep(400);
    const bad = await auditOverflow(p);
    console.log(`${width}px ${label}: ${bad.length === 0 ? 'OK' : bad.length + ' problem(s)'}`);
    for (const b of bad.slice(0, 8)) console.log(`   ${b.el} -> ${b.why} (right ${b.right} > ${b.vw})`);
    failures += bad.length;
    await p.close();
  };
  const go = (path, sel) => async (p, base) => {
    await p.goto(`${base}${path}`, { waitUntil: path === '/' ? 'load' : 'networkidle0' });
    await p.waitForSelector(sel);
  };
  await run('login', go('/', '#email'), LOGIN_APP);
  await run('list', go('/orbit/issues', 'li[data-issue-id]'));
  await run('detail ORB-1', go('/orbit/issue/ORB-1', '#issue-title'));
  await run('detail ORB-19 (sub-issues)', go('/orbit/issue/ORB-19', '#issue-title'));
  await run('board', go('/orbit/issues?view=board', '[data-column]'));
  await run('create', async (p, base) => {
    await go('/orbit/issues', 'li[data-issue-id]')(p, base);
    await p.click('button[aria-label="Create issue"]:not([title])');
    await p.waitForSelector('[role=dialog] #ci-title');
    await p.type('#ci-title', 'A very long title '.repeat(6));
    await p.type('#ci-desc', 'https://example.com/' + 'a'.repeat(120));
  });
  await run('palette', async (p, base) => {
    await go('/orbit/issues', 'li[data-issue-id]')(p, base);
    await p.click('button[aria-label="Search"]');
    await p.waitForSelector('[role=combobox]');
    await p.type('[role=combobox]', 'chat');
    await sleep(500);
  });
  await run('sidebar drawer', async (p, base) => {
    await go('/orbit/issues', 'li[data-issue-id]')(p, base);
    await p.click('button[aria-label="Open menu"]');
    await sleep(300);
  });
}
await browser.close();
if (failures) {
  console.error(`overflow audit FAILED: ${failures} element(s)`);
  process.exit(1);
}
console.log('overflow audit passed');

/* global document */
// Evidence screenshots of the web app with headless Chromium (Brave) via puppeteer-core.
// Needs: a Launchpad server filled with demo data (scripts/seed-demo.mjs), `vite` on :5173 with VITE_DEV_TOKEN set
// (signed-in views) and `vite preview` of a production build on :4173 (login screen). Phone shots use CDP device emulation.
import puppeteer from 'puppeteer-core';
import { mkdirSync } from 'node:fs';

const OUT = process.env.OUT ?? './screenshots/web';
const APP = process.env.APP ?? 'http://localhost:5173';
const LOGIN_APP = process.env.LOGIN_APP ?? 'http://localhost:4173';
const API = process.env.DEV_OPS_URL ?? 'http://localhost:18091';
const TOKEN = process.env.DEV_OPS_TOKEN;
mkdirSync(OUT, { recursive: true });

const browser = await puppeteer.launch({
  executablePath: process.env.CHROME ?? '/Applications/Brave Browser.app/Contents/MacOS/Brave Browser',
  headless: true,
  userDataDir: `${process.env.TMPDIR}/chrome-profile-shots`,
  args: ['--no-first-run', '--no-default-browser-check'],
});
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function newPage({ w = 1280, h = 800, dark = false, phone = false } = {}) {
  const page = await browser.newPage();
  page.setDefaultTimeout(60000);
  page.setDefaultNavigationTimeout(60000);
  if (phone)
    await page.setViewport({ width: w, height: h, deviceScaleFactor: 2, isMobile: true, hasTouch: true });
  else await page.setViewport({ width: w, height: h, deviceScaleFactor: 1 });
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: dark ? 'dark' : 'light' }]);
  page.on('pageerror', (e) => console.log('pageerror:', e.message));
  page.on('console', (m) => {
    if (['error'].includes(m.type())) console.log('console.error:', m.text());
  });
  return page;
}
const shot = async (page, name) => {
  await sleep(250);
  await page.screenshot({ path: `${OUT}/${name}.png` });
  console.log('saved', name);
};
const overflow = async (page, label) => {
  const r = await page.evaluate(() => ({
    sw: document.documentElement.scrollWidth,
    cw: document.documentElement.clientWidth,
  }));
  console.log(
    `${label}: scrollWidth=${r.sw} clientWidth=${r.cw} ${r.sw <= r.cw ? 'NO horizontal scroll' : 'HORIZONTAL SCROLL'}`,
  );
};

// 01 login (cookie mode: no dev token)
{
  const p = await newPage();
  await p.goto(`${LOGIN_APP}/`, { waitUntil: 'load' }); // a 401 on /api/v1/me keeps puppeteer's networkidle0 from settling
  await p.waitForSelector('#email');
  await p.type('#email', 'chen@example.com');
  await shot(p, '01-login');
  await p.close();
}
// 02/03 list light + dark
for (const [dark, name] of [
  [false, '02-issues-list-light'],
  [true, '03-issues-list-dark'],
]) {
  const p = await newPage({ dark });
  await p.goto(`${APP}/orbit/issues`, { waitUntil: 'networkidle0' });
  await p.waitForSelector('li[data-issue-id]');
  await p.keyboard.press('j');
  await p.keyboard.press('j');
  await shot(p, name);
  await p.close();
}
// 04 board
{
  const p = await newPage({ w: 1440 });
  await p.goto(`${APP}/orbit/issues?view=board`, { waitUntil: 'networkidle0' });
  await p.waitForSelector('[data-column]');
  await shot(p, '04-board');
  await p.close();
}
// 05 detail (comments, relations) light and dark; 06 sub-issues parent
{
  const p = await newPage({ h: 1100 });
  await p.goto(`${APP}/orbit/issue/ORB-1`, { waitUntil: 'networkidle0' });
  await p.waitForSelector('#issue-title');
  await sleep(500);
  await shot(p, '05-issue-detail');
  await p.close();
  const d = await newPage({ h: 1100, dark: true });
  await d.goto(`${APP}/orbit/issue/ORB-19`, { waitUntil: 'networkidle0' });
  await d.waitForSelector('#issue-title');
  await sleep(500);
  await shot(d, '06-issue-detail-subissues-dark');
  await d.close();
}
// 07 create modal
{
  const p = await newPage();
  await p.goto(`${APP}/orbit/issues`, { waitUntil: 'networkidle0' });
  await p.waitForSelector('li[data-issue-id]');
  await p.keyboard.press('c');
  await p.waitForSelector('[role=dialog] #ci-title');
  await p.type('#ci-title', 'Settings: let parents mute a chat for one hour');
  await p.type('#ci-desc', 'Add a **mute** option to the chat menu.\n\n- 1 hour\n- until tomorrow');
  await shot(p, '07-create-modal');
  await p.close();
}
// 08 command palette
{
  const p = await newPage();
  await p.goto(`${APP}/orbit/issues`, { waitUntil: 'networkidle0' });
  await p.waitForSelector('li[data-issue-id]');
  await p.keyboard.down('Meta');
  await p.keyboard.press('k');
  await p.keyboard.up('Meta');
  await p.waitForSelector('[role=combobox]');
  await p.type('[role=combobox]', 'chat');
  await sleep(700);
  await shot(p, '08-command-palette');
  await p.close();
}
// 09-12 phone width (375 x 812, CDP device emulation)
{
  const p = await newPage({ w: 375, h: 812, phone: true });
  await p.goto(`${APP}/orbit/issues`, { waitUntil: 'networkidle0' });
  await p.waitForSelector('li[data-issue-id]');
  await shot(p, '09-phone-list');
  await overflow(p, 'phone list');
  await p.click('button[aria-label="Open menu"]');
  await sleep(300);
  await shot(p, '10-phone-sidebar');
  await p.keyboard.press('Escape');
  await p.goto(`${APP}/orbit/issue/ORB-1`, { waitUntil: 'networkidle0' });
  await p.waitForSelector('#issue-title');
  await sleep(500);
  await shot(p, '11-phone-detail');
  await overflow(p, 'phone detail');
  await p.goto(`${APP}/orbit/issues`, { waitUntil: 'networkidle0' });
  await p.waitForSelector('li[data-issue-id]');
  await p.click('button[aria-label="Create issue"]:not([title])');
  await p.waitForSelector('[role=dialog] #ci-title');
  await p.type('#ci-title', 'Fix the keyboard covering the send button');
  await shot(p, '12-phone-create');
  await overflow(p, 'phone create');
  await p.keyboard.press('Escape');
  await p.click('button[aria-label="Search"]');
  await p.waitForSelector('[role=combobox]');
  await shot(p, '13-phone-palette');
  await overflow(p, 'phone palette');
  await p.keyboard.press('Escape');
  await p.goto(`${APP}/orbit/issues?view=board`, { waitUntil: 'networkidle0' });
  await p.waitForSelector('[data-column]');
  await shot(p, '14-phone-board');
  await overflow(p, 'phone board');
  await p.close();
}
// 15 live update: tab B changes a title through the API, tab A updates without a reload.
if (TOKEN) {
  const a = await newPage();
  await a.goto(`${APP}/orbit/issues`, { waitUntil: 'networkidle0' });
  await a.waitForSelector('li[data-issue-id]');
  const title = `Live update ${Date.now() % 100000}`;
  const res = await fetch(`${API}/api/v1/orgs/orbit/issues`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ team: 'ORB', title, priority: 2, state: 'In Progress' }),
  });
  if (!res.ok) throw new Error(`create failed ${res.status}`);
  const t0 = Date.now();
  await a.waitForFunction((t) => document.body.innerText.includes(t), { timeout: 20000 }, title);
  console.log(
    `LIVE: tab A showed the new issue "${title}" ${Date.now() - t0} ms after it was created elsewhere, with no reload`,
  );
  // Bring the new row into view so the screenshot shows it.
  await a.evaluate((t) => {
    const li = [...document.querySelectorAll('li[data-issue-id]')].find((e) => e.textContent.includes(t));
    li?.scrollIntoView({ block: 'center' });
  }, title);
  await shot(a, '15-live-update');
  await a.close();
}
await browser.close();

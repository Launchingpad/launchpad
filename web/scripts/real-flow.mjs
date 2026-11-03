/* global document, window */
// REAL end-to-end proof in cookie mode. No mocked fetch: Vite dev (no VITE_DEV_TOKEN) proxies /api to a
// real dev-ops server run with LOG_ENV=local and AUTH_FIXED_OTP=000000. A bearer token is used ONLY by this
// script (outside the browser) to read back what the browser wrote and to create an issue from "elsewhere".
//   APP=http://localhost:5173 API=http://localhost:18091 DEV_OPS_TOKEN=odt_... EMAIL=chen@example.com node scripts/real-flow.mjs
import puppeteer from 'puppeteer-core';
import { mkdirSync, rmSync } from 'node:fs';

const APP = process.env.APP ?? 'http://localhost:5173';
const API = process.env.API ?? 'http://localhost:18091';
const TOKEN = process.env.DEV_OPS_TOKEN;
const EMAIL = process.env.EMAIL ?? 'chen@example.com';
const OUT = process.env.OUT ?? './screenshots/web';
if (!TOKEN) throw new Error('set DEV_OPS_TOKEN (used only to read back and to create an issue)');
mkdirSync(OUT, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failed = 0;
const check = (ok, msg) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${msg}`);
  if (!ok) failed++;
};
const bearer = (path, init = {}) =>
  fetch(`${API}${path}`, {
    ...init,
    headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json', ...init.headers },
  });

rmSync(`${process.env.TMPDIR}/chrome-profile-real`, { recursive: true, force: true }); // fresh browser: no old cookies
const SLOW = Number(process.env.REAL_FLOW_TIMEOUT_MS ?? 30000); // raise on a loaded machine
const browser = await puppeteer.launch({
  protocolTimeout: SLOW * 4,
  timeout: SLOW,
  executablePath: process.env.CHROME ?? '/Applications/Brave Browser.app/Contents/MacOS/Brave Browser',
  headless: true,
  userDataDir: `${process.env.TMPDIR}/chrome-profile-real`,
  args: ['--no-first-run', '--no-default-browser-check'],
});
const page = await browser.newPage();
page.setDefaultTimeout(SLOW);
await page.setViewport({ width: 1280, height: 800 });
const reqs = [];
page.on('request', (r) => reqs.push(`${r.method()} ${new URL(r.url()).pathname}${new URL(r.url()).search}`));
page.on('pageerror', (e) => console.log('pageerror:', e.message));
const shot = async (name) => {
  await sleep(300);
  await page.screenshot({ path: `${OUT}/${name}.png` });
  console.log('saved', name);
};

// 1. Signed out: the app sends us to /login.
await page.goto(`${APP}/orbit/issues`, { waitUntil: 'load' });
await page.waitForSelector('#email');
check(new URL(page.url()).pathname === '/login', 'signed-out visit to /orbit/issues lands on /login');
check(
  (await page.evaluate(() => JSON.stringify(window.localStorage))) === '{}',
  'localStorage is empty before login',
);

// 2. Email, then code 000000.
await page.type('#email', EMAIL);
await shot('16-real-login-email');
await page.click('button[type=submit]');
await page.waitForSelector('#code');
await page.type('#code', '00000'); // the form submits as soon as the sixth digit is typed
await shot('17-real-login-code');
await page.type('#code', '0');
await page.waitForSelector('li[data-issue-id]');
check(
  new URL(page.url()).pathname === '/orbit/issues',
  'after the code, the issue list for the org in the route is shown',
);
const cookiesAfter = await page.cookies(APP);
const names = cookiesAfter.map((c) => c.name).sort();
console.log('cookie names:', names.join(', '));
const sess = cookiesAfter.find((c) => c.name === 'odt_session');
const csrf = cookiesAfter.find((c) => c.name === 'odt_csrf');
check(!!sess && sess.httpOnly, 'session cookie is set and HttpOnly (page JS cannot read it)');
check(!!csrf && !csrf.httpOnly, 'csrf cookie is set (readable)');
check(
  !(await page.evaluate(() => document.cookie)).includes('odt_session'),
  'document.cookie does not expose the session',
);
const stored = await page.evaluate(
  () => JSON.stringify({ ...window.localStorage }) + JSON.stringify({ ...window.sessionStorage }),
);
console.log('storage after login:', stored);
check(
  !/odt_|token|csrf|session/i.test(stored),
  'no token, csrf or session value in localStorage or sessionStorage after login',
);
await shot('18-real-logged-in-list');

// 3. Reload keeps the session.
await page.reload({ waitUntil: 'load' });
await page.waitForSelector('li[data-issue-id]');
check(new URL(page.url()).pathname === '/orbit/issues', 'reload keeps the session (no redirect to /login)');
check(
  reqs.some((r) => r === 'GET /api/v1/auth/session'),
  'the app rebuilt the session with GET /api/v1/auth/session',
);

// 4. CSRF: raw requests from the page with the real cookie session.
const raw = (withHeader) =>
  page.evaluate(async (withHeader) => {
    const c = document.cookie
      .split('; ')
      .find((x) => x.startsWith('odt_csrf='))
      ?.split('=')[1];
    const res = await fetch('/api/v1/orgs/orbit/labels', {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json', ...(withHeader ? { 'X-CSRF-Token': c } : {}) },
      body: JSON.stringify({
        name: withHeader ? 'csrf-ok-' + Date.now() : 'csrf-refused-' + Date.now(),
        color: '#112233',
      }),
    });
    return { status: res.status, body: await res.json().catch(() => null) };
  }, withHeader);
const noHeader = await raw(false);
check(
  noHeader.status === 403 && noHeader.body?.code === 'csrf_failed',
  `server refuses a cookie write with no CSRF header (${noHeader.status} ${noHeader.body?.code})`,
);
const withHeader = await raw(true);
check(
  withHeader.status === 201 || withHeader.status === 200,
  `server accepts the same write with the CSRF header (${withHeader.status})`,
);

// 5. A write through the app UI (comment on ORB-1), then read it back with the bearer token.
await page.goto(`${APP}/orbit/issue/ORB-1`, { waitUntil: 'load' });
await page.waitForSelector('#issue-title');
const text = `Real cookie-session comment ${Date.now() % 100000}`;
await page.type('textarea[placeholder="Leave a comment"]', text);
const putsBefore = reqs.length;
await page.evaluate(() =>
  [...document.querySelectorAll('button')].find((b) => b.textContent === 'Comment').click(),
);
await page.waitForFunction((t) => document.body.innerText.includes(t), {}, text);
const post = reqs.slice(putsBefore).find((r) => r.startsWith('POST') && r.includes('/comments'));
check(!!post, `the UI sent ${post}`);
const back = await (await bearer('/api/v1/orgs/orbit/issues/ORB-1/comments')).json();
check(
  back.items.some((c) => c.body === text),
  'the comment is on the real server (read back with the bearer token)',
);
await shot('19-real-write-comment');

// 6. Cookie-mode SSE: no polling, and an issue created elsewhere appears with no reload.
await page.goto(`${APP}/orbit/issues`, { waitUntil: 'load' });
await page.waitForSelector('li[data-issue-id]');
await sleep(1500);
const streamReqs = reqs.filter((r) => r.startsWith('GET ') && r.includes('/events/stream'));
check(streamReqs.length > 0, `the app opened the event stream: ${streamReqs[0] ?? 'none'}`);
const before = reqs.length;
const title = `Real SSE issue ${Date.now() % 100000}`;
const created = await bearer('/api/v1/orgs/orbit/issues', {
  method: 'POST',
  body: JSON.stringify({ team: 'ORB', title, priority: 2, state: 'In Progress' }),
});
check(created.ok, `an issue was created from outside the browser (${created.status})`);
const t0 = Date.now();
await page.waitForFunction((t) => document.body.innerText.includes(t), { timeout: 15000 }, title);
const ms = Date.now() - t0;
const polled = reqs.slice(before).filter((r) => /GET .*\/events\?after=/.test(r));
check(polled.length === 0, `no polling: ${polled.length} GET events?after= requests (SSE did the work)`);
console.log(`LIVE (cookie SSE): new issue shown ${ms} ms after creation, no reload`);
await page.evaluate((t) => {
  [...document.querySelectorAll('li[data-issue-id]')]
    .find((e) => e.textContent.includes(t))
    ?.scrollIntoView({ block: 'center' });
}, title);
await shot('20-real-sse-live-update');

// 7. Logout.
await page.evaluate(() =>
  [...document.querySelectorAll('button')].findLast((b) => b.textContent.includes('Chen')).click(),
);
await page.waitForSelector('[role=menuitem]');
await page.evaluate(() =>
  [...document.querySelectorAll('[role=menuitem]')].find((b) => b.textContent.includes('Sign out')).click(),
);
await page.waitForSelector('#email');
check(new URL(page.url()).pathname === '/login', 'sign out returns to /login');
check(
  (await page.cookies(APP)).every((c) => !c.name.startsWith('odt_') || !c.value),
  'both cookies are cleared',
);
const after = await page.evaluate(
  async () => (await fetch('/api/v1/auth/session', { credentials: 'include' })).status,
);
check(after === 401, `GET /auth/session is now ${after} (want 401)`);
await shot('21-real-logged-out');
await browser.close();
console.log(failed ? `real flow FAILED: ${failed} check(s)` : 'real flow passed');
process.exit(failed ? 1 : 0);

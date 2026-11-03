// Dev helper: node scripts/shot.mjs <url> <out.png> [width] [height] [dark]
import puppeteer from 'puppeteer-core';
const [url, out, w = '1280', h = '800', dark] = process.argv.slice(2);
const browser = await puppeteer.launch({
  executablePath: process.env.CHROME ?? '/Applications/Brave Browser.app/Contents/MacOS/Brave Browser',
  headless: true,
  userDataDir: process.env.TMPDIR + '/chrome-profile',
  args: ['--no-first-run', '--no-default-browser-check'],
});
const page = await browser.newPage();
await page.setViewport({ width: +w, height: +h, deviceScaleFactor: 1 });
if (dark) await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'dark' }]);
page.on('console', (m) => console.log('console:', m.type(), m.text()));
page.on('pageerror', (e) => console.log('pageerror:', e.message));
await page.goto(url, { waitUntil: 'networkidle0' });
await page.screenshot({ path: out });
await browser.close();

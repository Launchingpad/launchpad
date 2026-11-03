// After a production build: no dev token path and no bearer token in the bundle.
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

function* walk(d) {
  for (const f of readdirSync(d)) {
    const p = join(d, f);
    if (statSync(p).isDirectory()) yield* walk(p);
    else yield p;
  }
}
let bad = 0;
for (const f of walk('dist')) {
  if (!/\.(js|html|css|map)$/.test(f)) continue;
  const s = readFileSync(f, 'utf8');
  if (/odt_[A-Za-z0-9]{20,}/.test(s) || s.includes('VITE_DEV_TOKEN')) {
    console.error(`dev token reference found in ${f}`);
    bad++;
  }
  // LAUN-132: react-dom/server (about 22 KB gzip) must not be in the bundle.
  if (f.endsWith('.js') && /renderToStaticMarkup|renderToPipeableStream/.test(s)) {
    console.error(`react-dom/server found in ${f}`);
    bad++;
  }
}
if (bad) process.exit(1);
console.log('dist is free of dev token references and react-dom/server');

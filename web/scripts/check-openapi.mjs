// Fails when src/api/schema.d.ts is not what openapi-typescript makes from
// ../server/api/openapi.yaml. Run `npm run openapi` to refresh it.
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = mkdtempSync(join(tmpdir(), 'openapi-check-'));
const out = join(dir, 'schema.d.ts');
try {
  execFileSync('npx', ['--no-install', 'openapi-typescript', '../server/api/openapi.yaml', '-o', out], {
    stdio: 'pipe',
  });
  const fresh = readFileSync(out, 'utf8');
  const committed = readFileSync('src/api/schema.d.ts', 'utf8');
  if (fresh !== committed) {
    console.error('src/api/schema.d.ts is stale. Run `npm run openapi` and commit the result.');
    process.exit(1);
  }
  console.log('openapi client is current');
} finally {
  rmSync(dir, { recursive: true, force: true });
}

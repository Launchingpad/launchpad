import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

// LAUN-107: an inline mp4 loads through the API 302 to the files host, so media-src must allow the same
// origins as img-src. default-src is 'none', so a missing media-src kills the player.
describe('CSP template', () => {
  const csp = readFileSync('deploy/security.inc.template', 'utf8');
  const directive = (name: string) => new RegExp(`${name} ([^;"]*)`).exec(csp)?.[1];
  it('media-src has the same origins as img-src (minus data:)', () => {
    expect(directive('media-src')).toBe("'self' ${API_ORIGIN} ${FILES_ORIGIN}");
    expect(directive('img-src')).toBe("'self' data: ${API_ORIGIN} ${FILES_ORIGIN}");
  });
});

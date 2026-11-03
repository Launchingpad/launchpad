import { describe, expect, it } from 'vitest';
import { ApiError, cleanServerText, errorMessage, toApiError } from './errors';

describe('server error text', () => {
  it('keeps plain text and strips control characters', () => {
    expect(cleanServerText('bad\u0000 title\n\tgiven')).toBe('bad title given');
  });
  it('drops JSON and HTML bodies instead of showing them', () => {
    expect(cleanServerText('{"error":"x"}')).toBe('');
    expect(cleanServerText('<html><body>502</body></html>')).toBe('');
  });
  it('caps the length', () => {
    expect(cleanServerText('a'.repeat(500)).length).toBeLessThanOrEqual(200);
  });
});

describe('toApiError', () => {
  it('uses the server text and code', () => {
    const e = toApiError(new Response(null, { status: 400 }), {
      error: 'title is required',
      code: 'validation_failed',
    });
    expect(e).toMatchObject({ status: 400, code: 'validation_failed', message: 'title is required' });
  });
  it('falls back to a friendly message for a non-JSON body', () => {
    const e = toApiError(new Response(null, { status: 502 }), '<html>bad gateway</html>');
    expect(e.message).toBe('The server had a problem. Try again.');
    expect(errorMessage(e)).not.toContain('<');
  });
  it('reads Retry-After', () => {
    const e = toApiError(new Response(null, { status: 429, headers: { 'Retry-After': '42' } }), {});
    expect(e.retryAfter).toBe(42);
  });
  it('shows nothing raw for an unknown error', () => {
    expect(errorMessage(new Error('stack at foo.js:12'))).toBe('Something went wrong.');
    expect(errorMessage(new ApiError(0, 'network_error', 'Could not reach the server.'))).toContain(
      'Could not reach',
    );
  });
});

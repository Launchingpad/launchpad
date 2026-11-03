import { describe, expect, it } from 'vitest';
import { ApiError } from '../../../api/errors';
import { PROVIDERS, forbiddenText, gitErrorText } from './gitText';

const HOST_WORDS = 'secret-host-words glpat-ABCDEF';
const forbidden = () => new ApiError(502, 'provider_forbidden', HOST_WORDS);

describe('provider_forbidden text (LAUN-103)', () => {
  it('names the missing permission for the call and the provider, in fixed words', () => {
    for (const p of PROVIDERS) {
      const hook = gitErrorText(forbidden(), 'hook', p.id);
      expect(hook).toContain(p.needs.hook);
      expect(hook).toContain(p.label);
      expect(hook).toContain('usually');
      expect(gitErrorText(forbidden(), 'repos', p.id)).toContain(p.needs.hook);
      expect(gitErrorText(forbidden(), 'rotate', p.id)).toContain(p.needs.hook);
      expect(gitErrorText(forbidden(), 'list', p.id)).toContain(p.needs.list);
      expect(gitErrorText(forbidden(), 'check', p.id)).toContain(p.needs.check);
      expect(gitErrorText(forbidden(), 'connect', p.id)).toContain(p.needs.check);
      expect(gitErrorText(forbidden(), 'token', p.id)).toContain(p.needs.check);
      expect(gitErrorText(forbidden(), 'backfill', p.id)).toContain('read merge requests');
    }
  });

  it('says different things for hooks and for the repository list', () => {
    expect(gitErrorText(forbidden(), 'hook', 'github')).not.toBe(gitErrorText(forbidden(), 'list', 'github'));
    expect(gitErrorText(forbidden(), 'list', 'gitlab')).toContain('Projects');
  });

  it('never shows the words of the host', () => {
    for (const ctx of ['hook', 'list', 'check', 'connect', 'repos', 'backfill', 'other'] as const) {
      const t = gitErrorText(forbidden(), ctx, 'gitlab');
      expect(t).not.toContain('secret-host-words');
      expect(t).not.toContain('glpat');
    }
  });

  it('falls back to a general sentence when the provider is not known', () => {
    expect(forbiddenText('hook')).toContain('does not have the scopes it needs');
    expect(forbiddenText('hook', 'nope')).toContain('does not have the scopes it needs');
  });
});

describe('token help text (LAUN-103)', () => {
  it('GitLab shows the classic token and the fine-grained form, and marks the names as maybe wrong', () => {
    const gl = PROVIDERS.find((p) => p.id === 'gitlab')!;
    expect(gl.scopes.join(' ')).toContain('scope api');
    expect(gl.scopes.join(' ')).toContain('fine-grained form');
    expect(gl.scopes.join(' ')).toContain('Group and project');
    expect(gl.scopeNote).toContain('can be wrong');
  });

  it('builds no external address: only a path after the connection base URL', () => {
    for (const p of PROVIDERS) expect(p.tokenPage === '' || p.tokenPage.startsWith('/')).toBe(true);
  });
});

import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { clip, joinTitle, MAX_ITEM_TITLE, useAppTitle, useDocumentTitle } from './documentTitle';

function Page({ parts, prefix }: { parts: (string | null | undefined)[]; prefix?: string }) {
  useDocumentTitle(parts, prefix);
  return null;
}

describe('useDocumentTitle', () => {
  it('joins the parts with the Linear separator (U+203A) and updates when they change', () => {
    document.title = 'Launchpad 🚀';
    const { rerender } = render(<Page parts={['Orbit', 'ORB-1 One']} />);
    expect(document.title).toBe('Orbit › ORB-1 One');
    expect(document.title).toContain('›');
    rerender(<Page parts={['Orbit', 'ORB-1 Edited']} />);
    expect(document.title).toBe('Orbit › ORB-1 Edited');
  });

  it('skips empty parts, and with no part at all leaves the title alone', () => {
    document.title = 'Launchpad 🚀';
    const { rerender } = render(<Page parts={['Orbit', undefined, null, '']} />);
    expect(document.title).toBe('Orbit');
    rerender(<Page parts={[]} />);
    expect(document.title).toBe('Orbit');
  });

  it('restores nothing on unmount: the next page sets its own', () => {
    const { unmount } = render(<Page parts={['Orbit', 'Issues']} />);
    unmount();
    expect(document.title).toBe('Orbit › Issues');
  });

  it('has a place for the unread prefix', () => {
    render(<Page parts={['Orbit', 'Inbox']} prefix="(3) " />);
    expect(document.title).toBe('(3) Orbit › Inbox');
  });

  it('useAppTitle names Launchpad and the page', () => {
    function P() {
      useAppTitle('Log in');
      return null;
    }
    render(<P />);
    expect(document.title).toBe('Launchpad › Log in');
  });
});

describe('clip and joinTitle', () => {
  it('keeps a short title and cuts a long one at the limit with an ellipsis', () => {
    expect(clip('Short title')).toBe('Short title');
    expect(clip('x'.repeat(MAX_ITEM_TITLE))).toHaveLength(MAX_ITEM_TITLE);
    const cut = clip('word '.repeat(40));
    expect([...cut].length).toBeLessThanOrEqual(MAX_ITEM_TITLE);
    expect(cut.endsWith('…')).toBe(true);
  });
  it('collapses white space and counts code points, not UTF-16 units', () => {
    expect(clip('a\n  b')).toBe('a b');
    const cut = clip('🚀'.repeat(100));
    expect([...cut]).toHaveLength(MAX_ITEM_TITLE);
  });
  it('a whitespace-only text clips to nothing', () => {
    expect(clip(' \n\t ')).toBe('');
  });
  it('joins', () => {
    expect(joinTitle(['A', false, 'B'])).toBe('A › B');
  });
});

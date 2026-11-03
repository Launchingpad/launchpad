import { fireEvent, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';
import { baseRoutes, makeIssue, mockApi } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-50. jsdom has no layout, so the stretched ::after cannot be hit here: the real-browser check
// covers a click on the padding (docs/internal/web-decisions.md). These tests pin the structure and
// the click rule: one link per card, controls apart from it, modified clicks left alone.

const dt = () => ({ effectAllowed: '', dropEffect: '', setData: () => {}, getData: () => '' });

function setup() {
  const issue = makeIssue(1, { title: 'Open me' });
  mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [issue], nextCursor: null } }),
    'GET /api/v1/orgs/:org/issues/:issue': () => ({
      json: {
        ...issue,
        relations: { blocks: [], blockedBy: [], related: [], duplicateOf: [], duplicates: [] },
        stateHistory: [],
      },
    }),
  });
}

function narrow(on: boolean) {
  const orig = window.matchMedia;
  window.matchMedia = ((query: string) => ({
    matches: on,
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as typeof window.matchMedia;
  return () => {
    window.matchMedia = orig;
  };
}

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

describe.each([
  ['list row', '/orbit/issues'],
  ['board card', '/orbit/issues?view=board'],
])('%s', (_name, path) => {
  const item = () => document.querySelector('[data-issue-id]') as HTMLElement;

  it('has one link, named by the title, and no nested interactive content in it', async () => {
    setup();
    renderApp(path);
    await screen.findByText('Open me');
    const links = within(item()).getAllByRole('link');
    expect(links).toHaveLength(1);
    expect(links[0]).toHaveAccessibleName('Open me');
    expect(links[0]).toHaveAttribute('href', '/orbit/issue/ORB-1');
    expect(links[0]!.querySelector('a, button, input')).toBeNull();
  });

  it('a click on the title opens the issue', async () => {
    setup();
    const user = userEvent.setup();
    renderApp(path);
    await user.click(await screen.findByRole('link', { name: 'Open me' }));
    expect(await screen.findByLabelText('Title')).toBeInTheDocument();
  });

  it('the checkbox toggles and does not open the issue', async () => {
    setup();
    const user = userEvent.setup();
    renderApp(path);
    await screen.findByText('Open me');
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-1' }));
    expect(screen.getByText('1 selected')).toBeInTheDocument();
    expect(screen.queryByLabelText('Title')).not.toBeInTheDocument();
  });

  it('a click with Ctrl or Cmd is left to the browser (new tab), even in select mode', async () => {
    setup();
    const restore = narrow(true);
    try {
      const user = userEvent.setup();
      renderApp(path);
      const link = await screen.findByRole('link', { name: 'Open me' });
      await user.click(screen.getByRole('button', { name: 'Select' }));
      // Our handler must not prevent these. A document listener sees the event after it,
      // records that, then stops jsdom from trying to follow the link.
      const seen: boolean[] = [];
      const spy = (e: Event) => {
        seen.push(e.defaultPrevented);
        e.preventDefault();
      };
      document.addEventListener('click', spy);
      fireEvent.click(link, { ctrlKey: true });
      fireEvent.click(link, { metaKey: true });
      fireEvent.click(link, { button: 1 });
      document.removeEventListener('click', spy);
      expect(seen).toEqual([false, false, false]);
      expect(screen.queryByText('1 selected')).not.toBeInTheDocument();
    } finally {
      restore();
    }
  });

  it('on a phone in select mode a tap toggles the selection and does not navigate', async () => {
    setup();
    const restore = narrow(true);
    try {
      const user = userEvent.setup();
      renderApp(path);
      const link = await screen.findByRole('link', { name: 'Open me' });
      await user.click(screen.getByRole('button', { name: 'Select' }));
      await user.click(link);
      expect(screen.getByText('1 selected')).toBeInTheDocument();
      expect(screen.queryByLabelText('Title')).not.toBeInTheDocument();
      await user.click(link);
      expect(screen.queryByText('1 selected')).not.toBeInTheDocument();
    } finally {
      restore();
    }
  });

  it('a click whose target is the item itself (press and release on different children) does not open it', async () => {
    setup();
    renderApp(path);
    await screen.findByText('Open me');
    fireEvent.click(item());
    expect(screen.queryByLabelText('Title')).not.toBeInTheDocument();
  });

  it('a plain click on the identifier opens the issue (forwarded to the link)', async () => {
    setup();
    const user = userEvent.setup();
    renderApp(path);
    await user.click(await screen.findByText('ORB-1'));
    expect(await screen.findByLabelText('Title')).toBeInTheDocument();
  });

  it('A mouse click opens the issue when the selected text is outside this item', async () => {
    setup();
    renderApp(path);
    const link = await screen.findByRole('link', { name: 'Open me' });
    const range = document.createRange();
    range.selectNodeContents(document.body.querySelector('h2')!);
    window.getSelection()!.removeAllRanges();
    window.getSelection()!.addRange(range);
    fireEvent.click(link, { detail: 1 }); // a mouse click: the selection is outside this item
    expect(await screen.findByLabelText('Title')).toBeInTheDocument();
    window.getSelection()!.removeAllRanges();
  });

  it('a mouse click after a text selection inside the item does not open it; Enter still does', async () => {
    setup();
    renderApp(path);
    const link = await screen.findByRole('link', { name: 'Open me' });
    const range = document.createRange();
    range.selectNodeContents(link);
    window.getSelection()!.removeAllRanges();
    window.getSelection()!.addRange(range);
    fireEvent.click(link, { detail: 1 });
    expect(screen.queryByLabelText('Title')).not.toBeInTheDocument();
    fireEvent.click(link, { detail: 0 });
    expect(await screen.findByLabelText('Title')).toBeInTheDocument();
    window.getSelection()!.removeAllRanges();
  });
});

describe('board drag', () => {
  // The pointer drag and its click guard are in boardDrag.test.tsx. Here: the card has no native drag.
  it('a card has no browser drag (no ghost image): dragstart is cancelled', async () => {
    setup();
    renderApp('/orbit/issues?view=board');
    const link = await screen.findByRole('link', { name: 'Open me' });
    const card = link.closest('li')!;
    expect(card).not.toHaveAttribute('draggable');
    expect(fireEvent.dragStart(link, { dataTransfer: dt() })).toBe(false); // preventDefault was called
    expect(screen.queryByLabelText('Title')).not.toBeInTheDocument();
  });
});

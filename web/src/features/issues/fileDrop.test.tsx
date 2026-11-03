import { fireEvent, screen, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import { fileMarkdown } from './useIssueFileDrop';
import { baseRoutes, makeIssue, mockApi, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-58: files dropped on the issue page upload through the paperclip's code and attach to the issue.
const emptyRel = { blocks: [], blockedBy: [], related: [], duplicateOf: [], duplicates: [] };
const dt = (files: File[], types: string[] = ['Files']) => ({ types, files, dropEffect: 'none' });
const txt = (name: string) => new File(['hi'], name, { type: 'text/plain' });
const png = (name: string) => new File(['png'], name, { type: 'image/png' });

function setup(over: Record<string, Handler> = {}) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [], nextCursor: null } }),
    'GET /api/v1/orgs/:org/issues/:issue': () => ({
      json: {
        ...makeIssue(1, { description: 'Some text' }),
        relations: emptyRel,
        stateHistory: [],
        attachments: [],
      },
    }),
    'GET /api/v1/orgs/:org/issues/:issue/comments': () => ({ json: { items: [], nextCursor: null } }),
    'GET /api/v1/orgs/:org/projects': () => ({ json: { items: [], nextCursor: null } }),
    'PUT /up': () => ({ status: 200 }),
    'POST /api/v1/orgs/:org/attachments/prepare': () => ({
      json: {
        uploadKey: 'org/1/2026/a/f',
        uploadUrl: 'https://silo.test/up',
        method: 'PUT',
        headers: {},
        contentType: 'text/plain',
        sizeBytes: 2,
        expiresAt: '2026-10-26T00:00:00Z',
      },
    }),
    'POST /api/v1/orgs/:org/issues/:issue/attachments': (r) => ({
      status: 201,
      json: {
        id: 'a1',
        kind: 'file',
        filename: (r.body as { filename: string }).filename,
        downloadUrl: '/api/v1/orgs/orbit/attachments/a1/download',
      },
    }),
    ...over,
  });
}
const posts = (api: ReturnType<typeof setup>, tail: string) =>
  api.calls.filter((c) => c.method === 'POST' && c.url.pathname.endsWith(tail));

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

describe('drop files on the issue page', () => {
  it('shows the overlay for files, not for text, and clears on leave', async () => {
    setup();
    renderApp('/orbit/issue/ORB-1');
    const zone = await screen.findByText('Some text');
    fireEvent.dragEnter(zone, { dataTransfer: dt([], ['text/plain']) });
    expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
    fireEvent.dragEnter(zone, { dataTransfer: dt([]) });
    expect(screen.getByTestId('drop-overlay')).toBeInTheDocument();
    fireEvent.dragLeave(zone, { dataTransfer: dt([]) });
    expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
  });

  it('uploads and attaches several files, and refreshes the issue', async () => {
    const api = setup();
    renderApp('/orbit/issue/ORB-1');
    const body = await screen.findByText('Some text');
    const before = api.calls.filter(
      (c) => c.method === 'GET' && c.url.pathname.endsWith('/issues/ORB-1'),
    ).length;
    fireEvent.drop(body, { dataTransfer: dt([txt('a.txt'), txt('b.txt')]) });
    await waitFor(() => expect(posts(api, '/issues/ORB-1/attachments')).toHaveLength(2));
    expect(await screen.findByText('2 files attached.')).toBeInTheDocument();
    await waitFor(() =>
      expect(
        api.calls.filter((c) => c.method === 'GET' && c.url.pathname.endsWith('/issues/ORB-1')).length,
      ).toBeGreaterThan(before),
    );
  });

  it('a refused file is named and the others still upload', async () => {
    const api = setup();
    renderApp('/orbit/issue/ORB-1');
    const body = await screen.findByText('Some text');
    fireEvent.drop(body, {
      dataTransfer: dt([txt('a.txt'), new File(['x'], 'run.exe', { type: 'application/x-msdownload' })]),
    });
    expect(await screen.findByText(/Not attached: run\.exe \(type not allowed/)).toBeInTheDocument();
    await waitFor(() => expect(posts(api, '/issues/ORB-1/attachments')).toHaveLength(1));
  });

  it('attachments off: one fixed message', async () => {
    setup({
      'POST /api/v1/orgs/:org/attachments/prepare': () => ({
        status: 503,
        json: { error: 'x', code: 'attachments_unavailable' },
      }),
    });
    renderApp('/orbit/issue/ORB-1');
    fireEvent.drop(await screen.findByText('Some text'), { dataTransfer: dt([txt('a.txt'), txt('b.txt')]) });
    expect(await screen.findByText('Attachments are turned off on this server.')).toBeInTheDocument();
  });

  it('a drop of text does nothing', async () => {
    const api = setup();
    renderApp('/orbit/issue/ORB-1');
    fireEvent.drop(await screen.findByText('Some text'), { dataTransfer: dt([], ['text/plain']) });
    await new Promise((r) => setTimeout(r, 30));
    expect(posts(api, '/attachments/prepare')).toHaveLength(0);
  });

  it('a drop on the comment box attaches and writes markdown (image and link) with the download URL', async () => {
    const api = setup();
    renderApp('/orbit/issue/ORB-1');
    const box = (await screen.findByLabelText('Leave a comment')) as HTMLTextAreaElement;
    fireEvent.drop(box, { dataTransfer: dt([png('shot.png'), txt('notes.txt')]) });
    await waitFor(() => expect(posts(api, '/issues/ORB-1/attachments')).toHaveLength(2));
    await waitFor(() => expect(box.value).toContain('notes.txt'));
    expect(box.value).toBe(
      '![shot.png](/api/v1/orgs/orbit/attachments/a1/download)\n[notes.txt](/api/v1/orgs/orbit/attachments/a1/download)\n',
    );
    expect(box.value).not.toContain('silo.test');
  });
});

describe('one drop is one attachment (LAUN-105)', () => {
  it('the same file listed twice in one drop attaches once', async () => {
    const api = setup();
    renderApp('/orbit/issue/ORB-1');
    const body = await screen.findByText('Some text');
    const a = png('Screenshot.png');
    const b = new File(['png'], 'Screenshot.png', { type: 'image/png', lastModified: a.lastModified });
    fireEvent.drop(body, { dataTransfer: dt([a, b]) });
    expect(await screen.findByText('File attached.')).toBeInTheDocument();
    expect(posts(api, '/attachments/prepare')).toHaveLength(1);
    expect(posts(api, '/issues/ORB-1/attachments')).toHaveLength(1);
  });

  it('one file dropped on a comment box (nested in the zone) attaches once', async () => {
    const api = setup();
    renderApp('/orbit/issue/ORB-1');
    const box = await screen.findByLabelText('Leave a comment');
    fireEvent.drop(box, { dataTransfer: dt([png('shot.png')]) });
    expect(await screen.findByText('File attached.')).toBeInTheDocument();
    expect(posts(api, '/issues/ORB-1/attachments')).toHaveLength(1);
  });

  it('the same native drop event is handled once', async () => {
    const api = setup();
    renderApp('/orbit/issue/ORB-1');
    const body = await screen.findByText('Some text');
    const ev = new Event('drop', { bubbles: true, cancelable: true });
    Object.assign(ev, { dataTransfer: dt([png('shot.png')]) });
    body.dispatchEvent(ev);
    body.dispatchEvent(ev);
    expect(await screen.findByText('File attached.')).toBeInTheDocument();
    expect(posts(api, '/issues/ORB-1/attachments')).toHaveLength(1);
  });

  it('the same file dropped twice on purpose (two drops) attaches twice', async () => {
    const api = setup();
    renderApp('/orbit/issue/ORB-1');
    const body = await screen.findByText('Some text');
    const f = png('same.png');
    fireEvent.drop(body, { dataTransfer: dt([f]) });
    await waitFor(() => expect(posts(api, '/issues/ORB-1/attachments')).toHaveLength(1));
    fireEvent.drop(body, { dataTransfer: dt([f]) });
    await waitFor(() => expect(posts(api, '/issues/ORB-1/attachments')).toHaveLength(2));
  });
});

describe('the drop overlay covers the pane (LAUN-105)', () => {
  it('names the issue and sits beside the scroller, not inside it', async () => {
    setup();
    renderApp('/orbit/issue/ORB-1');
    const zone = await screen.findByText('Some text');
    fireEvent.dragEnter(zone, { dataTransfer: dt([]) });
    const overlay = screen.getByTestId('drop-overlay');
    expect(overlay).toHaveTextContent('Drop files to attach to ORB-1');
    // The scroller is the next sibling: an overlay inside it would scroll away with the content.
    expect(overlay.nextElementSibling?.contains(zone)).toBe(true);
    expect(overlay.parentElement?.contains(zone)).toBe(true);
  });

  it('survives nested enter/leave without flicker', async () => {
    setup();
    renderApp('/orbit/issue/ORB-1');
    const box = await screen.findByLabelText('Leave a comment');
    const zone = await screen.findByText('Some text');
    fireEvent.dragEnter(zone, { dataTransfer: dt([]) });
    fireEvent.dragEnter(box, { dataTransfer: dt([]) });
    fireEvent.dragLeave(zone, { dataTransfer: dt([]) });
    expect(screen.getByTestId('drop-overlay')).toBeInTheDocument();
    fireEvent.dragLeave(box, { dataTransfer: dt([]) });
    expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
  });
});

describe('overlay hardening and markdown names', () => {
  it('clears on mousemove, pointermove, blur and visibilitychange', async () => {
    setup();
    renderApp('/orbit/issue/ORB-1');
    const zone = await screen.findByText('Some text');
    const ends = [
      () => fireEvent.mouseMove(window),
      () => fireEvent.pointerMove(window),
      () => fireEvent.blur(window),
      () => fireEvent(document, new Event('visibilitychange')),
    ];
    for (const end of ends) {
      fireEvent.dragEnter(zone, { dataTransfer: dt([]) });
      expect(screen.getByTestId('drop-overlay')).toBeInTheDocument();
      end();
      expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
    }
  });

  it('fileMarkdown collapses whitespace, strips brackets and backticks, falls back to "file"', () => {
    const f = (n: string, t = 'text/plain') => new File(['x'], n, { type: t });
    expect(fileMarkdown(f('a\nb  c`d].txt'), '/u')).toBe('[a b cd.txt](/u)');
    expect(fileMarkdown(f('x.png', 'image/png'), '/u')).toBe('![x.png](/u)');
    expect(fileMarkdown(f('[]`'), '/u')).toBe('[file](/u)');
    expect(fileMarkdown(f('  ', 'image/png'), '/u')).toBe('![file](/u)');
  });
});

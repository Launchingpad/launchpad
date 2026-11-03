import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, makeIssue, mockApi } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-133: a bare ticket number goes to the server as `query=113`, and the
// server's order (the ticket first, then title matches) is shown as it comes.
function setup() {
  const ticket = makeIssue(113, { title: 'The ticket itself' });
  const titled = makeIssue(7, { title: 'Fix 113 retries' });
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': (r) =>
      r.url.searchParams.get('query') === '113'
        ? { json: { items: [ticket, titled], nextCursor: null } }
        : { json: { items: [makeIssue(1)], nextCursor: null } },
    'GET /api/v1/orgs/:org/issues/:issue': (r) => ({
      json: {
        ...makeIssue(1),
        identifier: r.params.issue,
        relations: { blocks: [], blockedBy: [], related: [], duplicateOf: [], duplicates: [] },
        stateHistory: [],
      },
    }),
  });
  return api;
}

const queries = (calls: { url: URL }[]) =>
  calls.map((c) => c.url.searchParams.get('query')).filter((q) => q !== null);

describe('searching by ticket number', () => {
  it('the command palette sends the bare number and lists the ticket first', async () => {
    const api = setup();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    await user.keyboard('{Meta>}k{/Meta}');
    await user.type(await screen.findByRole('combobox', { name: 'Search issues and commands' }), '113');
    const opts = await screen.findAllByRole('option', { name: /ORB-/ });
    expect(opts[0]).toHaveTextContent('ORB-113');
    expect(opts[1]).toHaveTextContent('ORB-7');
    expect(queries(api.calls)).toContain('113');
  });

  it('an issue picker sends the bare number and lists the ticket first', async () => {
    const api = setup();
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: /Add relation/ }));
    await user.type(await screen.findByPlaceholderText('Search by ID or title…'), '113');
    await waitFor(() => expect(queries(api.calls)).toContain('113'));
    const opts = await screen.findAllByRole('option', { name: /ORB-/ });
    expect(opts[0]).toHaveTextContent('ORB-113');
    expect(opts[1]).toHaveTextContent('ORB-7');
  });
});

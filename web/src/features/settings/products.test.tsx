import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Product } from '../../api/types';
import { baseRoutes, mockApi, type Handler, type MockRequest } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { CAP, CHANGED, SLUG_NEEDED, SLUG_RULE, SLUG_TAKEN } from './productText';

function product(over: Partial<Product> = {}): Product {
  return {
    id: 'p1',
    slug: 'launchpad',
    name: 'Launchpad',
    description: '',
    icon: null,
    color: null,
    version: 1,
    teamKeys: [],
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    archivedAt: null,
    ...over,
  };
}

/** A small stateful products API: the list answers from `rows`, the team calls change them. */
function setup(opts: { role?: 'admin' | 'member'; rows?: Product[]; routes?: Record<string, Handler> } = {}) {
  const rows = opts.rows ?? [product()];
  const base = baseRoutes();
  const api = mockApi({
    ...base,
    'GET /api/v1/auth/session': (r) => {
      const s = base['GET /api/v1/auth/session']!(r) as { json: { orgs: { role: string }[] } };
      s.json.orgs[0]!.role = opts.role ?? 'admin';
      return s;
    },
    'GET /api/v1/orgs/:org/products': (r) => ({
      json: {
        items: rows.filter((p) => r.url.searchParams.get('includeArchived') === 'true' || !p.archivedAt),
        nextCursor: null,
      },
    }),
    'PUT /api/v1/orgs/:org/products/:product/teams/:team': (r) => {
      const p = rows.find((x) => x.slug === r.params.product)!;
      p.teamKeys = [...p.teamKeys, r.params.team!];
      return { status: 204 };
    },
    'DELETE /api/v1/orgs/:org/products/:product/teams/:team': (r) => {
      const p = rows.find((x) => x.slug === r.params.product)!;
      p.teamKeys = p.teamKeys.filter((k) => k !== r.params.team);
      return { status: 204 };
    },
    ...opts.routes,
  });
  return { api, rows };
}
const sent = (api: ReturnType<typeof mockApi>, method: string, re: RegExp): MockRequest[] =>
  api.calls.filter((c) => c.method === method && re.test(c.url.pathname));

describe('Settings, Products (LAUN-66)', () => {
  it('a member gets the admin notice and the page makes no archived-list request', async () => {
    const { api } = setup({ role: 'member' });
    renderApp('/orbit/settings/products');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
    // The sidebar (LAUN-64) reads the plain list for every member; only this page asks for archived ones.
    expect(
      sent(api, 'GET', /\/products$/).filter((c) => c.url.searchParams.has('includeArchived')),
    ).toHaveLength(0);
  });

  it('lists products and asks for archived ones only when "Include archived" is ticked', async () => {
    const { api } = setup({
      rows: [
        product({ teamKeys: ['ORB'] }),
        product({ id: 'p2', slug: 'old', name: 'Old', archivedAt: '2026-10-02T00:00:00Z' }),
      ],
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    const list = await screen.findByRole('list', { name: 'Products' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(1);
    expect(within(list).getByText(/ORB/)).toBeInTheDocument();
    expect(sent(api, 'GET', /\/products$/)[0]!.url.searchParams.has('includeArchived')).toBe(false);
    await user.click(screen.getByRole('checkbox', { name: 'Include archived' }));
    await waitFor(() =>
      expect(
        within(screen.getByRole('list', { name: 'Products', hidden: true })).getAllByRole('listitem'),
      ).toHaveLength(2),
    );
    expect(
      sent(api, 'GET', /\/products$/)
        .at(-1)!
        .url.searchParams.get('includeArchived'),
    ).toBe('true');
    expect(screen.getByText('Archived')).toBeInTheDocument();
  });

  it('does not show the slug in a row (LAUN-151)', async () => {
    setup({ rows: [product({ teamKeys: ['ORB'] })] });
    renderApp('/orbit/settings/products');
    const list = await screen.findByRole('list', { name: 'Products' });
    expect(within(list).getByText('ORB')).toBeInTheDocument();
    expect(within(list).queryByText('launchpad')).toBeNull();
    expect(within(list).queryByText(/·/)).toBeNull();
  });

  it('shortens a long team list with +N and keeps every key in the tooltip (LAUN-100)', async () => {
    const keys = ['A1', 'B2', 'C3', 'D4', 'E5', 'F6', 'G7', 'H8'];
    setup({ rows: [product({ name: 'Wide', teamKeys: keys })] });
    renderApp('/orbit/settings/products');
    const list = await screen.findByRole('list', { name: 'Products' });
    const short = within(list).getByText('A1, B2, C3, D4, E5, F6 +2');
    expect(short).toHaveAttribute('title', `Wide: teams ${keys.join(', ')}`);
  });

  it('shows the empty state', async () => {
    setup({ rows: [] });
    renderApp('/orbit/settings/products');
    expect(await screen.findByText('No products yet')).toBeInTheDocument();
  });

  it('creates a product and sends no slug when the field is empty', async () => {
    const { api, rows } = setup({
      routes: {
        'POST /api/v1/orgs/:org/products': (r) => {
          rows.push(product({ id: 'p9', slug: 'billing', name: (r.body as { name: string }).name }));
          return { status: 201, json: rows.at(-1) };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Create a product' }));
    await user.type(screen.getByLabelText('Name'), 'Billing');
    await user.click(screen.getByRole('button', { name: 'Create product' }));
    await screen.findByText('Billing');
    expect(sent(api, 'POST', /\/products$/)[0]!.body).toEqual({ name: 'Billing' });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('a taken slug shows the fixed 409 sentence under the slug field', async () => {
    setup({
      routes: {
        'POST /api/v1/orgs/:org/products': () => ({
          status: 409,
          json: {
            error: 'product slug launchpad is already used in this org (server words)',
            code: 'conflict',
          },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Create a product' }));
    await user.type(screen.getByLabelText('Name'), 'Other');
    await user.type(screen.getByLabelText('Slug'), 'launchpad');
    await user.click(screen.getByRole('button', { name: 'Create product' }));
    expect(await screen.findByText(SLUG_TAKEN)).toBeInTheDocument();
    expect(screen.queryByText(/server words/)).not.toBeInTheDocument();
    expect(screen.getByLabelText('Slug')).toHaveFocus();
  });

  it('a name with no Latin letters asks for a slug', async () => {
    setup({
      routes: {
        'POST /api/v1/orgs/:org/products': () => ({
          status: 400,
          json: { error: 'cannot make a slug from this name; send a slug', code: 'validation_failed' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Create a product' }));
    await user.type(screen.getByLabelText('Name'), '日本語');
    await user.click(screen.getByRole('button', { name: 'Create product' }));
    expect(await screen.findByText(SLUG_NEEDED)).toBeInTheDocument();
    expect(screen.getByLabelText('Slug')).toHaveAttribute('aria-invalid', 'true');
  });

  it('checks a bad slug and colour on the page, before any request', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Create a product' }));
    await user.type(screen.getByLabelText('Name'), 'X');
    await user.type(screen.getByLabelText('Slug'), '9bad');
    await user.type(screen.getByLabelText('Colour'), 'red');
    await user.click(screen.getByRole('button', { name: 'Create product' }));
    expect(await screen.findByText(/Use a colour like/)).toBeInTheDocument();
    expect(screen.getByText(/Start with a letter/)).toBeInTheDocument();
    expect(sent(api, 'POST', /\/products$/)).toHaveLength(0);
  });

  it('edit sends only the changed field and the version', async () => {
    const { api } = setup({
      rows: [product({ version: 4, description: 'Old text' })],
      routes: { 'PATCH /api/v1/orgs/:org/products/:product': () => ({ json: product({ version: 5 }) }) },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Edit Launchpad' }));
    const name = screen.getByLabelText('Name');
    await user.clear(name);
    await user.type(name, 'Launchpad 2');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(sent(api, 'PATCH', /products\/launchpad$/)).toHaveLength(1));
    expect(sent(api, 'PATCH', /products\/launchpad$/)[0]!.body).toEqual({ version: 4, name: 'Launchpad 2' });
  });

  it('an archived product has its name and slug disabled and no Save button', async () => {
    setup({ rows: [product({ archivedAt: '2026-10-02T00:00:00Z' })] });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('checkbox', { name: 'Include archived' }));
    await user.click(await screen.findByRole('button', { name: 'Edit Launchpad' }));
    expect(screen.getByLabelText('Name')).toBeDisabled();
    expect(screen.getByLabelText('Slug')).toBeDisabled();
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
  });

  it('archive sends archived:true with the version; restore sends archived:false', async () => {
    const rows = [product({ version: 3 })];
    const { api } = setup({
      rows,
      routes: {
        'PATCH /api/v1/orgs/:org/products/:product': (r) => {
          rows[0] = product({
            version: 4,
            archivedAt: (r.body as { archived: boolean }).archived ? '2026-10-02T00:00:00Z' : null,
          });
          return { json: rows[0] };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('checkbox', { name: 'Include archived' }));
    await user.click(await screen.findByRole('button', { name: 'Archive Launchpad' }));
    await screen.findByRole('button', { name: 'Restore Launchpad' });
    expect(sent(api, 'PATCH', /products\/launchpad$/)[0]!.body).toEqual({ archived: true, version: 3 });
    await user.click(screen.getByRole('button', { name: 'Restore Launchpad' }));
    await screen.findByRole('button', { name: 'Archive Launchpad' });
    expect(sent(api, 'PATCH', /products\/launchpad$/)[1]!.body).toEqual({ archived: false, version: 4 });
  });

  it('the page says an archived product can be deleted, and the dialog says only archived ones can', async () => {
    setup({ rows: [product({ archivedAt: '2026-10-02T00:00:00Z' })] });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    expect(
      await screen.findByText(/Archive a product first; an archived product can then be deleted/),
    ).toBeInTheDocument();
    await user.click(await screen.findByRole('checkbox', { name: 'Include archived' }));
    await user.click(await screen.findByRole('button', { name: 'Delete Launchpad' }));
    expect(
      within(screen.getByRole('dialog')).getByText(/Only an archived product can be deleted/),
    ).toBeInTheDocument();
  });

  it('delete is offered only for an archived product, and asks first', async () => {
    const rows = [
      product(),
      product({ id: 'p2', slug: 'old', name: 'Old', archivedAt: '2026-10-02T00:00:00Z' }),
    ];
    const { api } = setup({
      rows,
      routes: {
        'DELETE /api/v1/orgs/:org/products/:product': (r) => {
          rows.splice(
            rows.findIndex((p) => p.slug === r.params.product),
            1,
          );
          return { status: 204 };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('checkbox', { name: 'Include archived' }));
    await screen.findByRole('button', { name: 'Delete Old' });
    expect(screen.queryByRole('button', { name: 'Delete Launchpad' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Delete Old' }));
    expect(sent(api, 'DELETE', /products\/old$/)).toHaveLength(0);
    await user.click(screen.getByRole('button', { name: 'Delete product' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Delete Old' })).not.toBeInTheDocument());
    expect(sent(api, 'DELETE', /products\/old$/)).toHaveLength(1);
  });

  it('ticking and unticking a team saves it and the page list refreshes', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Assign teams to Launchpad' }));
    const box = within(screen.getByRole('dialog')).getByRole('checkbox', { name: /ORB/ });
    expect(box).not.toBeChecked();
    await user.click(box);
    await waitFor(() => expect(box).toBeChecked());
    expect(sent(api, 'PUT', /products\/launchpad\/teams\/ORB$/)).toHaveLength(1);
    expect(
      within(screen.getByRole('list', { name: 'Products', hidden: true })).getByText(/ORB/),
    ).toBeInTheDocument();
    await user.click(box);
    await waitFor(() => expect(box).not.toBeChecked());
    expect(sent(api, 'DELETE', /products\/launchpad\/teams\/ORB$/)).toHaveLength(1);
    await waitFor(() =>
      expect(
        within(screen.getByRole('list', { name: 'Products', hidden: true })).getByText(/No teams/),
      ).toBeInTheDocument(),
    );
  });

  it('a failed assign shows a fixed sentence and the box follows the server', async () => {
    setup({
      routes: {
        'PUT /api/v1/orgs/:org/products/:product/teams/:team': () => ({
          status: 500,
          json: { error: 'secret server words', code: 'internal' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Assign teams to Launchpad' }));
    const box = within(screen.getByRole('dialog')).getByRole('checkbox', { name: /ORB/ });
    await user.click(box);
    expect(await screen.findByText('ORB: Could not add the team. Try again.')).toBeInTheDocument();
    expect(box).not.toBeChecked();
  });

  it('the team checklist of an archived product is disabled', async () => {
    setup({ rows: [product({ archivedAt: '2026-10-02T00:00:00Z' })] });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('checkbox', { name: 'Include archived' }));
    await user.click(await screen.findByRole('button', { name: 'Assign teams to Launchpad' }));
    expect(within(screen.getByRole('dialog')).getByRole('checkbox', { name: /ORB/ })).toBeDisabled();
  });

  it('a version conflict shows its sentence, refetches the list, and the next Save sends the new version', async () => {
    const rows = [product({ version: 4 })];
    let first = true;
    const { api } = setup({
      rows,
      routes: {
        'PATCH /api/v1/orgs/:org/products/:product': () => {
          if (first) {
            first = false;
            rows[0] = product({ version: 5, description: 'Changed by someone' });
            return { status: 409, json: { error: 'version mismatch', code: 'version_conflict' } };
          }
          return { json: product({ version: 6 }) };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Edit Launchpad' }));
    const gets = () => sent(api, 'GET', /\/products$/).length;
    const before = gets();
    await user.clear(screen.getByLabelText('Name'));
    await user.type(screen.getByLabelText('Name'), 'Renamed');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/Someone else changed this product/)).toBeInTheDocument();
    await waitFor(() => expect(gets()).toBeGreaterThan(before));
    // The typed name is kept and the dialog says the row changed.
    expect(screen.getByLabelText('Name')).toHaveValue('Renamed');
    expect(await screen.findByText(/changed in the background/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(sent(api, 'PATCH', /products\/launchpad$/)).toHaveLength(2));
    expect(sent(api, 'PATCH', /products\/launchpad$/)[1]!.body).toEqual({ version: 5, name: 'Renamed' });
  });

  it('the 200-product cap has its own sentence; any other 409 is the generic one, not "slug taken"', async () => {
    let msg = 'an org can have at most 200 products, archived ones included';
    setup({
      routes: {
        'POST /api/v1/orgs/:org/products': () => ({ status: 409, json: { error: msg, code: 'conflict' } }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Create a product' }));
    await user.type(screen.getByLabelText('Name'), 'Z');
    await user.click(screen.getByRole('button', { name: 'Create product' }));
    expect(await screen.findByText(CAP)).toBeInTheDocument();
    msg = 'product is archived; restore it first';
    await user.click(screen.getByRole('button', { name: 'Create product' }));
    expect(await screen.findByText(CHANGED)).toBeInTheDocument();
    expect(screen.queryByText(SLUG_TAKEN)).not.toBeInTheDocument();
    expect(screen.queryByText(CAP)).not.toBeInTheDocument();
  });

  it('an empty slug on edit is refused before any request', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Edit Launchpad' }));
    await user.clear(screen.getByLabelText('Slug'));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(SLUG_RULE)).toBeInTheDocument();
    expect(sent(api, 'PATCH', /products/)).toHaveLength(0);
  });

  it('the archived edit dialog starts with focus on Restore, and Escape closes it', async () => {
    setup({ rows: [product({ archivedAt: '2026-10-02T00:00:00Z' })] });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('checkbox', { name: 'Include archived' }));
    await user.click(await screen.findByRole('button', { name: 'Edit Launchpad' }));
    expect(within(screen.getByRole('dialog')).getByRole('button', { name: 'Restore' })).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('Escape closes the create dialog and the teams dialog', async () => {
    setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Create a product' }));
    expect(screen.getByLabelText('Name')).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Assign teams to Launchpad' }));
    await within(screen.getByRole('dialog')).findByRole('checkbox', { name: /ORB/ });
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('the delete confirm opens with focus on Cancel, and Escape closes it without a request', async () => {
    const { api } = setup({ rows: [product({ archivedAt: '2026-10-02T00:00:00Z' })] });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('checkbox', { name: 'Include archived' }));
    await user.click(await screen.findByRole('button', { name: 'Delete Launchpad' }));
    expect(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(sent(api, 'DELETE', /products/)).toHaveLength(0);
  });

  it('focus stays on the team checkbox after a tick, and a tick during a save is ignored', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const { api } = setup({
      routes: {
        'PUT /api/v1/orgs/:org/products/:product/teams/:team': async () => {
          await gate;
          return { status: 204 };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Assign teams to Launchpad' }));
    const box = await within(screen.getByRole('dialog')).findByRole('checkbox', { name: /ORB/ });
    await user.click(box);
    await waitFor(() => expect(box).toHaveAttribute('aria-disabled', 'true'));
    expect(box).toHaveFocus();
    await user.click(box);
    expect(sent(api, 'PUT', /teams\/ORB$/)).toHaveLength(1);
    release();
    await waitFor(() => expect(box).not.toHaveAttribute('aria-disabled'));
    expect(box).toHaveFocus();
  });

  it('the teams dialog says "Loading teams" in a status region while the teams load', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    setup({
      routes: {
        'GET /api/v1/orgs/:org/teams': async () => {
          await gate;
          return { json: { items: [], nextCursor: null } };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/products');
    await user.click(await screen.findByRole('button', { name: 'Assign teams to Launchpad' }));
    expect(await within(screen.getByRole('dialog')).findByRole('status')).toHaveTextContent('Loading teams');
    release();
  });
});

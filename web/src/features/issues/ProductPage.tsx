import { useCallback, useMemo } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useProduct } from '../../api/products';
import { ApiError } from '../../api/errors';
import { usePageTitle } from '../../lib/documentTitle';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { useIsOrgAdmin } from '../settings/access';
import { safeColor } from '../shell/sidebarGroups';
import { useUi } from '../shell/ui-context';
import { IssueBrowser } from './IssueBrowser';
import { parseFilters, writeFilters, type IssueFilters } from './filters';
import { placeKey, useRememberedView } from './viewPrefs';
import ui from '../../styles/ui.module.css';
import styles from './issues.module.css';
import own from './product.module.css';

/**
 * `/:org/product/:slug` (LAUN-65, decisions-T18 #24, #27): the issues of all the teams of a product, as a list or
 * a board. The product is sent to the server as `extra.product` (not as a filter), so it is not a chip, not in the
 * filter count and not in the URL; the server's `product` filter also takes archived teams.
 */
export function ProductPage() {
  const { slug = '' } = useParams();
  const product = useProduct(slug.toLowerCase());
  const missing = product.error instanceof ApiError && product.error.status === 404;
  usePageTitle(missing ? 'Not found' : product.data?.name);

  if (product.isPending) return <SkeletonRows />;
  if (missing) return <EmptyState title="Page not found" />;
  if (product.isError)
    return (
      <ErrorState
        error={product.error}
        onRetry={() => void product.refetch()}
        title="Could not load product"
      />
    );
  return <ProductBody p={product.data} />;
}

function ProductBody({ p }: { p: NonNullable<ReturnType<typeof useProduct>['data']> }) {
  const org = useOrgSlug();
  const [sp, setSp] = useSearchParams();
  const { openCreate } = useUi();
  const isAdmin = useIsOrgAdmin();
  const archived = p.archivedAt !== null;
  const keys = useMemo(() => p.teams.map((t) => t.key), [p.teams]);

  // The product and the team are fixed by the page: a stray ?product= or ?team= in the address is dropped.
  const parsed = useMemo(() => parseFilters(sp), [sp]);
  const write = useCallback(
    (f: IssueFilters) =>
      setSp(writeFilters({ ...f, product: undefined, team: undefined }), { replace: true }),
    [setSp],
  );
  // LAUN-109: the layout and the sort are remembered for each product.
  const remembered = useRememberedView(placeKey('product', p.slug), sp, parsed, write);
  const filters: IssueFilters = useMemo(
    () => ({ ...remembered.filters, product: undefined, team: undefined }),
    [remembered.filters],
  );
  const setFilters = remembered.setFilters;
  const extra = useMemo(() => ({ product: p.slug }), [p.slug]);
  const create = () => openCreate({ firstTeams: keys });
  const color = safeColor(p.color);

  const settingsLink = isAdmin ? (
    <Link to={`/${org}/settings/products`}>Assign teams in Settings, Products</Link>
  ) : null;

  const head = (
    <div className={own.head}>
      <div className={own.titleRow}>
        {(p.icon || color) && (
          <span className={own.mark} aria-hidden="true">
            {color && <span className={own.dot} style={{ background: color }} />}
            {p.icon}
          </span>
        )}
        <h1 className={`${styles.h1} ${own.title}`}>{p.name}</h1>
        <span className={own.spacer} />
        {!archived && keys.length > 0 && (
          <button type="button" className={`${ui.btn} ${ui.primary} ${own.headBtn}`} onClick={create}>
            Create issue
          </button>
        )}
        {isAdmin && (
          <Link to={`/${org}/settings/products`} className={`${ui.btn} ${own.headBtn}`}>
            Manage
          </Link>
        )}
      </div>
      {p.description && <p className={own.description}>{p.description}</p>}
      {p.teams.length > 0 && (
        <ul className={own.teams} aria-label="Teams in this product">
          {p.teams.map((t) => (
            <li key={t.id}>
              <Link to={`/${org}/team/${t.key}`} className={own.team}>
                <span className={own.teamKey}>{t.key}</span>
                <span className={own.teamName}>{t.name}</span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );

  const banner = archived && (
    <div className={own.archived} role="status">
      This product is archived. It is read-only here. An admin can restore it in Settings, Products.
    </div>
  );

  // A product with no (live) team has no issue list to show; the issue list query is not sent.
  if (keys.length === 0)
    return (
      <div className={`${styles.page} ${own.page}`}>
        {head}
        {banner}
        <EmptyState title="This product has no teams yet">
          <div className={own.empty}>
            <p>A product shows the issues of its teams. It has no team now.</p>
            {archived ? null : settingsLink ? (
              <p>{settingsLink}</p>
            ) : (
              <p>Ask an organisation admin to add teams to this product.</p>
            )}
          </div>
        </EmptyState>
      </div>
    );

  return (
    <div className={`${styles.page} ${own.page}`}>
      {head}
      {banner}
      <IssueBrowser
        filters={filters}
        onChange={setFilters}
        onResetView={remembered.customised ? remembered.resetView : undefined}
        columns={remembered.columns}
        onColumnsChange={remembered.setColumns}
        extra={extra}
        lockProduct
        scopeTeams={keys}
        emptyTitle="No issues yet"
        emptyNone={
          archived ? undefined : (
            <button type="button" className={`${ui.btn} ${ui.primary}`} onClick={create}>
              Create the first issue
            </button>
          )
        }
      />
    </div>
  );
}

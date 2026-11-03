import { productTeamsTitle, teamKeysShort } from '../../ui/teamKeys';
import { useState } from 'react';
import { useProductList, useUpdateProduct } from '../../api/products';
import type { Product } from '../../api/types';
import ui from '../../styles/ui.module.css';
import { EmptyState, SkeletonRows } from '../../ui/States';
import { FixedError } from './FixedError';
import { DeleteProductDialog, ProductFormDialog, ProductTeamsDialog, touchClass } from './ProductDialogs';
import { actionText } from './productText';
import styles from './products.module.css';
import { SettingsCard, SettingsPage } from './SettingsParts';

type Open = { kind: 'create' } | { kind: 'edit' | 'teams' | 'delete'; slug: string } | null;

/** Settings, Products (LAUN-66, decisions-T18 #24). Admin only: SettingsLayout keeps everyone else out. */
export function ProductsPage() {
  const [showArchived, setShowArchived] = useState(false);
  const products = useProductList(showArchived);
  const update = useUpdateProduct();
  const [open, setOpen] = useState<Open>(null);
  const [error, setError] = useState<string | null>(null);

  const current =
    open && open.kind !== 'create' ? products.data?.find((p) => p.slug === open.slug) : undefined;
  const close = () => setOpen(null);

  function setArchived(p: Product, archived: boolean) {
    setError(null);
    update.mutate(
      { product: p.slug, body: { archived, version: p.version } },
      {
        onError: (e) =>
          setError(`${p.name}: ${actionText(e, archived ? 'archive the product' : 'restore the product')}`),
      },
    );
  }

  return (
    <SettingsPage
      title="Products"
      description="A product groups teams. A team can be in more than one product. Products show in the sidebar and filter the issue list. Archive a product first; an archived product can then be deleted. Show archived products with “Include archived”."
    >
      <div className={touchClass}>
        <div className={styles.toolbar}>
          <button
            type="button"
            className={`${ui.btn} ${ui.primary}`}
            onClick={() => setOpen({ kind: 'create' })}
          >
            Create a product
          </button>
          <label className={styles.check}>
            <input
              type="checkbox"
              checked={showArchived}
              onChange={(e) => setShowArchived(e.target.checked)}
            />
            <span>Include archived</span>
          </label>
        </div>
        {error && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            {error}
          </p>
        )}
        {products.isPending ? (
          <SkeletonRows n={3} />
        ) : products.isError ? (
          <FixedError
            error={products.error}
            onRetry={() => void products.refetch()}
            title="Could not load products"
          />
        ) : products.data.length === 0 ? (
          <EmptyState title="No products yet">
            <p>Create the first product, then assign teams to it.</p>
          </EmptyState>
        ) : (
          <SettingsCard>
            <ul className={styles.list} aria-label="Products">
              {products.data.map((p) => (
                <ProductRow
                  key={p.id}
                  p={p}
                  busy={update.isPending}
                  onOpen={(kind) => setOpen({ kind, slug: p.slug })}
                  onArchive={(a) => setArchived(p, a)}
                />
              ))}
            </ul>
          </SettingsCard>
        )}
      </div>
      {open?.kind === 'create' && <ProductFormDialog onClose={close} />}
      {open?.kind === 'edit' && current && (
        <ProductFormDialog
          key={current.id}
          product={current}
          onClose={close}
          onRestore={() => {
            setArchived(current, false);
            close();
          }}
        />
      )}
      {open?.kind === 'teams' && current && <ProductTeamsDialog product={current} onClose={close} />}
      {open?.kind === 'delete' && current?.archivedAt && (
        <DeleteProductDialog product={current} onClose={close} />
      )}
    </SettingsPage>
  );
}

function ProductRow({
  p,
  busy,
  onOpen,
  onArchive,
}: {
  p: Product;
  busy: boolean;
  onOpen: (kind: 'edit' | 'teams' | 'delete') => void;
  onArchive: (archived: boolean) => void;
}) {
  const archived = !!p.archivedAt;
  return (
    <li className={styles.item}>
      <span
        className={styles.swatch}
        style={p.color ? { background: p.color } : undefined}
        aria-hidden="true"
      >
        {p.icon}
      </span>
      <div className={styles.text}>
        <span className={styles.name}>
          {p.name}
          {archived && <span className={styles.tag}>Archived</span>}
        </span>
        <span className={styles.meta}>
          <span title={productTeamsTitle(p.name, p.teamKeys)}>{teamKeysShort(p.teamKeys, 6)}</span>
        </span>
      </div>
      <div className={styles.rowActions}>
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost}`}
          aria-label={`Assign teams to ${p.name}`}
          onClick={() => onOpen('teams')}
        >
          Assign teams
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost}`}
          aria-label={`Edit ${p.name}`}
          onClick={() => onOpen('edit')}
        >
          Edit
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost}`}
          aria-label={`${archived ? 'Restore' : 'Archive'} ${p.name}`}
          disabled={busy}
          onClick={() => onArchive(!archived)}
        >
          {archived ? 'Restore' : 'Archive'}
        </button>
        {archived && (
          <button
            type="button"
            className={`${ui.btn} ${ui.danger}`}
            aria-label={`Delete ${p.name}`}
            onClick={() => onOpen('delete')}
          >
            Delete
          </button>
        )}
      </div>
    </li>
  );
}

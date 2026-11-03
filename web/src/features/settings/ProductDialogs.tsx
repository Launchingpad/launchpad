import { useState, type FormEvent } from 'react';
import { useCreateProduct, useDeleteProduct, useSetProductTeam, useUpdateProduct } from '../../api/products';
import { useTeams } from '../../api/queries';
import type { Product, UpdateProductInput } from '../../api/types';
import ui from '../../styles/ui.module.css';
import { Dialog } from '../../ui/Dialog';
import {
  COLOR_RE,
  COLOR_RULE,
  ICON_RULE,
  NAME_RULE,
  SLUG_RE,
  SLUG_RULE,
  actionText,
  saveError,
  type ProductField,
} from './productText';
import styles from './products.module.css';
import settings from './settings.module.css';

/** On the page and on every dialog of it: the 40px touch rules of products.module.css. */
export const touchClass = styles.touch;

type Errors = Partial<Record<ProductField, string>>;

/** Create (`product` absent) or edit. An archived product shows its fields, locked, and a Restore button. */
export function ProductFormDialog({
  product,
  onClose,
  onRestore,
}: {
  product?: Product;
  onClose: () => void;
  onRestore?: () => void;
}) {
  const create = useCreateProduct();
  const update = useUpdateProduct();
  const archived = !!product?.archivedAt;
  const [name, setName] = useState(product?.name ?? '');
  const [slug, setSlug] = useState(product?.slug ?? '');
  const [description, setDescription] = useState(product?.description ?? '');
  const [icon, setIcon] = useState(product?.icon ?? '');
  const [color, setColor] = useState(product?.color ?? '');
  const [errors, setErrors] = useState<Errors>({});
  const [banner, setBanner] = useState<string | null>(null);
  const pending = create.isPending || update.isPending;
  // The row can change in the background (a refetch). Typed fields are kept; the note says so.
  const [opened] = useState(product);
  const openedVersion = opened?.version;
  const changed = !!product && product.version !== openedVersion;

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBanner(null);
    const n = name.trim();
    const s = slug.trim();
    const i = icon.trim();
    const c = color.trim();
    const found: Errors = {};
    if (n.length < 1 || [...n].length > 200) found.name = NAME_RULE;
    if ((s || product) && (s.length > 40 || !SLUG_RE.test(s))) found.slug = SLUG_RULE;
    if ([...i].length > 8) found.icon = ICON_RULE;
    if (c && !COLOR_RE.test(c)) found.color = COLOR_RULE;
    setErrors(found);
    const first = (['name', 'slug', 'icon', 'color'] as const).find((f) => found[f]);
    if (first) {
      document.getElementById(`product-${first}`)?.focus();
      return;
    }
    try {
      if (!product) {
        await create.mutateAsync({
          name: n,
          ...(s && { slug: s }),
          ...(description.trim() && { description: description.trim() }),
          ...(i && { icon: i }),
          ...(c && { color: c }),
        });
      } else {
        const body: UpdateProductInput = { version: product.version };
        if (n !== opened!.name) body.name = n;
        if (s !== opened!.slug) body.slug = s;
        if (description.trim() !== opened!.description) body.description = description.trim();
        if (i !== (opened!.icon ?? '')) body.icon = i || null;
        if (c !== (opened!.color ?? '')) body.color = c || null;
        if (Object.keys(body).length > 1) await update.mutateAsync({ product: product.slug, body });
      }
      onClose();
    } catch (err) {
      const r = saveError(err, !!s, product ? 'save' : 'create');
      if (r.field) {
        setErrors({ [r.field]: r.text });
        document.getElementById(`product-${r.field}`)?.focus();
      } else setBanner(r.text);
    }
  }

  const err = (f: ProductField) =>
    errors[f] && (
      <span id={`product-${f}-error`} className={settings.fieldError} aria-live="polite">
        {errors[f]}
      </span>
    );
  const attrs = (f: ProductField) => ({
    id: `product-${f}`,
    'aria-invalid': !!errors[f],
    'aria-describedby': errors[f] ? `product-${f}-error` : undefined,
    disabled: archived,
    autoComplete: 'off',
  });

  return (
    <Dialog
      className={touchClass}
      title={product ? `Edit ${product.name}` : 'Create a product'}
      width={520}
      onClose={onClose}
    >
      <form onSubmit={(e) => void submit(e)} noValidate>
        {archived && (
          <p className={ui.hint}>
            This product is archived. Restore it to edit it.{' '}
            {onRestore && (
              <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onRestore} data-autofocus>
                Restore
              </button>
            )}
          </p>
        )}
        {changed && (
          <p role="status" className={ui.hint}>
            This product changed in the background. Your edits are kept. Check them, then save.
          </p>
        )}
        <div className={ui.field}>
          <label htmlFor="product-name">Name</label>
          <input
            {...attrs('name')}
            className={ui.input}
            value={name}
            onChange={(e) => setName(e.target.value)}
            data-autofocus={archived ? undefined : true}
          />
          {err('name')}
        </div>
        <div className={ui.field}>
          <label htmlFor="product-slug">Slug</label>
          <input
            {...attrs('slug')}
            className={ui.input}
            value={slug}
            onChange={(e) => setSlug(e.target.value.toLowerCase())}
            spellCheck={false}
            autoCapitalize="none"
          />
          {err('slug')}
          <span className={ui.hint}>
            The slug is the product&apos;s name in links, for example /orbit/product/my-product.
            {!product && ' Leave it empty to make it from the name.'}
            {product && ' A new slug breaks old links.'}
          </span>
        </div>
        <div className={ui.field}>
          <label htmlFor="product-description">Description</label>
          <textarea
            id="product-description"
            className={ui.textarea}
            rows={3}
            disabled={archived}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>
        <div className={settings.pair}>
          <div className={ui.field}>
            <label htmlFor="product-icon">Icon</label>
            <input
              {...attrs('icon')}
              className={ui.input}
              value={icon}
              onChange={(e) => setIcon(e.target.value)}
              placeholder="Up to 8 characters"
            />
            {err('icon')}
          </div>
          <div className={ui.field}>
            <label htmlFor="product-color">Colour</label>
            <div className={styles.colorRow}>
              <input
                {...attrs('color')}
                className={ui.input}
                value={color}
                onChange={(e) => setColor(e.target.value)}
                placeholder="#5e6ad2"
                spellCheck={false}
              />
              {COLOR_RE.test(color.trim()) && (
                <span className={styles.preview} style={{ background: color.trim() }} aria-hidden="true" />
              )}
            </div>
            {err('color')}
          </div>
        </div>
        {!product && <p className={ui.hint}>You assign teams after you create the product.</p>}
        {banner && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            {banner}
          </p>
        )}
        <div className={styles.footer}>
          <button
            type="button"
            className={`${ui.btn} ${ui.ghost}`}
            onClick={onClose}
            data-autofocus={archived && !onRestore ? true : undefined}
          >
            {archived ? 'Close' : 'Cancel'}
          </button>
          {!archived && (
            <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={pending}>
              {pending ? 'Saving…' : product ? 'Save' : 'Create product'}
            </button>
          )}
        </div>
      </form>
    </Dialog>
  );
}

/** The checklist of all teams. Each tick is saved at once; the page list and the teams refresh. */
export function ProductTeamsDialog({ product, onClose }: { product: Product; onClose: () => void }) {
  const teams = useTeams();
  const set = useSetProductTeam();
  const [error, setError] = useState<string | null>(null);
  const archived = !!product.archivedAt;

  function toggle(team: string, member: boolean) {
    // One call at a time. The box is aria-disabled, not disabled, so focus stays on it; a second tick is ignored.
    if (set.isPending) return;
    setError(null);
    set.mutate(
      { product: product.slug, team, member },
      { onError: (e) => setError(`${team}: ${actionText(e, member ? 'add the team' : 'remove the team')}`) },
    );
  }

  return (
    <Dialog className={touchClass} title={`Assign teams to ${product.name}`} width={460} onClose={onClose}>
      {archived && <p className={ui.hint}>This product is archived. Restore it to change its teams.</p>}
      {teams.isPending ? (
        <p role="status" className={ui.hint}>
          Loading teams…
        </p>
      ) : teams.isError ? (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          Could not load the teams. Close this window and try again.
        </p>
      ) : teams.data.length === 0 ? (
        <p className={ui.hint}>The org has no teams yet. Create a team first.</p>
      ) : (
        <div className={styles.checkList} role="group" aria-label="Teams">
          {teams.data.map((t) => (
            <label key={t.id} className={styles.check}>
              <input
                type="checkbox"
                checked={product.teamKeys.includes(t.key)}
                disabled={archived}
                aria-disabled={set.isPending || undefined}
                onChange={(e) => toggle(t.key, e.target.checked)}
              />
              <span>
                <code>{t.key}</code> {t.name}
              </span>
            </label>
          ))}
        </div>
      )}
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      <div className={styles.footer}>
        <button type="button" className={`${ui.btn} ${ui.primary}`} onClick={onClose}>
          Done
        </button>
      </div>
    </Dialog>
  );
}

/** Delete asks first. The server refuses a product that is not archived; the page offers it for archived ones only. */
export function DeleteProductDialog({ product, onClose }: { product: Product; onClose: () => void }) {
  const del = useDeleteProduct();
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog className={touchClass} title={`Delete ${product.name}?`} width={460} onClose={onClose}>
      <p>
        This deletes the product for good. Only an archived product can be deleted. Its teams, their issues
        and their projects stay.
      </p>
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      <div className={styles.footer}>
        <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose} data-autofocus>
          Cancel
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.danger}`}
          disabled={del.isPending}
          onClick={() =>
            del.mutate(product.slug, {
              onSuccess: onClose,
              onError: (e) => setError(actionText(e, 'delete the product')),
            })
          }
        >
          {del.isPending ? 'Deleting…' : 'Delete product'}
        </button>
      </div>
    </Dialog>
  );
}

import { useMemo, useState, type FormEvent } from 'react';
import {
  useCreateProjectLabel,
  useDeleteProjectLabel,
  useProjectLabels,
  useUpdateProjectLabel,
} from '../../api/projectLabels';
import type { ProjectLabel } from '../../api/types';
import ui from '../../styles/ui.module.css';
import { Dialog } from '../../ui/Dialog';
import { EmptyState, SkeletonRows } from '../../ui/States';
import { LabelDot } from '../../ui/LabelPill';
import { FixedError } from './FixedError';
import {
  COLOR_RE,
  COLOR_RULE,
  DEFAULT_COLOR,
  NAME_RULE,
  actionText,
  saveError,
  type ProjectLabelField,
} from './projectLabelText';
import styles from './products.module.css';
import settings from './settings.module.css';
import own from './projectLabels.module.css';
import { SettingsCard, SettingsPage } from './SettingsParts';

type Open = { kind: 'create' } | { kind: 'edit' | 'delete'; id: string } | null;

/** The labels in display order: each group, then the labels in it; then the labels with no group, by name. */
export function orderLabels(all: ProjectLabel[]): { label: ProjectLabel; child: boolean }[] {
  const byName = (a: ProjectLabel, b: ProjectLabel) =>
    a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });
  const top = all.filter((l) => !l.parentId).sort(byName);
  const out: { label: ProjectLabel; child: boolean }[] = [];
  for (const t of top) {
    out.push({ label: t, child: false });
    for (const c of all.filter((l) => l.parentId === t.id).sort(byName)) out.push({ label: c, child: true });
  }
  return out;
}

/** Settings, Project labels (LAUN-194, decisions-T28). Admin only: SettingsLayout keeps everyone else out. */
export function ProjectLabelsPage() {
  const labels = useProjectLabels();
  const [open, setOpen] = useState<Open>(null);
  const rows = useMemo(() => orderLabels(labels.data ?? []), [labels.data]);
  const current = open && open.kind !== 'create' ? labels.data?.find((l) => l.id === open.id) : undefined;
  const close = () => setOpen(null);

  return (
    <SettingsPage
      title="Project labels"
      description="Project labels sort and filter projects. They are for the whole org. Put a label in a group to keep related labels together. Deleting a label takes it off every project."
    >
      <div className={styles.touch}>
        <div className={styles.toolbar}>
          <button
            type="button"
            className={`${ui.btn} ${ui.primary}`}
            onClick={() => setOpen({ kind: 'create' })}
          >
            Create a label
          </button>
        </div>
        {labels.isPending ? (
          <SkeletonRows n={3} />
        ) : labels.isError ? (
          <FixedError
            error={labels.error}
            onRetry={() => void labels.refetch()}
            title="Could not load project labels"
          />
        ) : rows.length === 0 ? (
          <EmptyState title="No project labels yet">
            <p>Create the first label, then put it on a project.</p>
          </EmptyState>
        ) : (
          <SettingsCard>
            <ul className={`${styles.list} ${own.list}`} aria-label="Project labels">
              {rows.map(({ label, child }) => (
                <li key={label.id} className={`${styles.item} ${child ? own.child : ''}`}>
                  <LabelDot color={label.color} />
                  <div className={styles.text}>
                    <span className={`${styles.name} ${own.name}`}>
                      {label.name}
                      {rows.some((r) => r.label.parentId === label.id) && (
                        <span className={styles.tag}>Group</span>
                      )}
                    </span>
                    <span className={styles.meta}>
                      {label.projectCount === 1 ? '1 project' : `${label.projectCount} projects`}
                    </span>
                  </div>
                  <div className={styles.rowActions}>
                    <button
                      type="button"
                      className={`${ui.btn} ${ui.ghost}`}
                      aria-label={`Edit ${label.name}`}
                      onClick={() => setOpen({ kind: 'edit', id: label.id })}
                    >
                      Edit
                    </button>
                    <button
                      type="button"
                      className={`${ui.btn} ${ui.danger}`}
                      aria-label={`Delete ${label.name}`}
                      onClick={() => setOpen({ kind: 'delete', id: label.id })}
                    >
                      Delete
                    </button>
                  </div>
                </li>
              ))}
            </ul>
          </SettingsCard>
        )}
      </div>
      {open?.kind === 'create' && <LabelFormDialog all={labels.data ?? []} onClose={close} />}
      {open?.kind === 'edit' && current && (
        <LabelFormDialog key={current.id} label={current} all={labels.data ?? []} onClose={close} />
      )}
      {open?.kind === 'delete' && current && <DeleteLabelDialog label={current} onClose={close} />}
    </SettingsPage>
  );
}

type Errors = Partial<Record<ProjectLabelField, string>>;

/** Create (`label` absent) or edit: name, colour and group. Only a top-level label without children can join a group. */
function LabelFormDialog({
  label,
  all,
  onClose,
}: {
  label?: ProjectLabel;
  all: ProjectLabel[];
  onClose: () => void;
}) {
  const create = useCreateProjectLabel();
  const update = useUpdateProjectLabel();
  const [name, setName] = useState(label?.name ?? '');
  const [color, setColor] = useState(label?.color ?? DEFAULT_COLOR);
  const [parent, setParent] = useState(label?.parentId ?? '');
  const [errors, setErrors] = useState<Errors>({});
  const [banner, setBanner] = useState<string | null>(null);
  const pending = create.isPending || update.isPending;
  const hasChildren = !!label && all.some((l) => l.parentId === label.id);
  const groups = all.filter((l) => !l.parentId && l.id !== label?.id);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBanner(null);
    const n = name.trim();
    const c = color.trim();
    const found: Errors = {};
    if (n.length < 1 || [...n].length > 100) found.name = NAME_RULE;
    if (!COLOR_RE.test(c)) found.color = COLOR_RULE;
    setErrors(found);
    const first = (['name', 'color'] as const).find((f) => found[f]);
    if (first) {
      document.getElementById(`plabel-${first}`)?.focus();
      return;
    }
    try {
      if (!label) {
        await create.mutateAsync({ name: n, color: c, ...(parent && { parentId: parent }) });
      } else {
        const body: Record<string, unknown> = {};
        if (n !== label.name) body.name = n;
        if (c.toLowerCase() !== label.color.toLowerCase()) body.color = c;
        if (parent !== (label.parentId ?? '')) body.parentId = parent || null;
        if (Object.keys(body).length > 0) await update.mutateAsync({ id: label.id, body });
      }
      onClose();
    } catch (err) {
      const r = saveError(err, label ? 'save' : 'create');
      if (r.field) {
        setErrors({ [r.field]: r.text });
        document.getElementById(`plabel-${r.field}`)?.focus();
      } else setBanner(r.text);
    }
  }

  const err = (f: ProjectLabelField) =>
    errors[f] && (
      <span id={`plabel-${f}-error`} className={settings.fieldError} aria-live="polite">
        {errors[f]}
      </span>
    );
  const attrs = (f: ProjectLabelField) => ({
    id: `plabel-${f}`,
    'aria-invalid': !!errors[f],
    'aria-describedby': errors[f] ? `plabel-${f}-error` : undefined,
    autoComplete: 'off',
  });

  return (
    <Dialog
      className={styles.touch}
      title={label ? `Edit ${label.name}` : 'Create a project label'}
      width={460}
      onClose={onClose}
    >
      <form onSubmit={(e) => void submit(e)} noValidate>
        <div className={ui.field}>
          <label htmlFor="plabel-name">Name</label>
          <input
            {...attrs('name')}
            className={ui.input}
            value={name}
            onChange={(e) => setName(e.target.value)}
            data-autofocus
          />
          {err('name')}
        </div>
        <div className={ui.field}>
          <label htmlFor="plabel-color">Colour</label>
          <div className={styles.colorRow}>
            <input
              {...attrs('color')}
              className={ui.input}
              value={color}
              onChange={(e) => setColor(e.target.value)}
              placeholder={DEFAULT_COLOR}
              spellCheck={false}
            />
            {COLOR_RE.test(color.trim()) && (
              <span className={styles.preview} style={{ background: color.trim() }} aria-hidden="true" />
            )}
          </div>
          {err('color')}
        </div>
        <div className={ui.field}>
          <label htmlFor="plabel-group">Group</label>
          <select
            id="plabel-group"
            className={ui.select}
            value={parent}
            disabled={hasChildren}
            onChange={(e) => setParent(e.target.value)}
          >
            <option value="">No group</option>
            {groups.map((g) => (
              <option key={g.id} value={g.id}>
                {g.name}
              </option>
            ))}
          </select>
          {hasChildren && (
            <span className={ui.hint}>This label is a group. Move its labels out to put it in a group.</span>
          )}
        </div>
        {banner && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            {banner}
          </p>
        )}
        <div className={styles.footer}>
          <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={pending}>
            {pending ? 'Saving…' : label ? 'Save' : 'Create label'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function DeleteLabelDialog({ label, onClose }: { label: ProjectLabel; onClose: () => void }) {
  const del = useDeleteProjectLabel();
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog className={styles.touch} title={`Delete ${label.name}?`} width={460} onClose={onClose}>
      <p>
        This deletes the label and takes it off{' '}
        {label.projectCount === 1 ? '1 project' : `${label.projectCount} projects`}. The projects stay.
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
            del.mutate(label.id, {
              onSuccess: onClose,
              onError: (e) => setError(actionText(e, 'delete the label')),
            })
          }
        >
          {del.isPending ? 'Deleting…' : 'Delete label'}
        </button>
      </div>
    </Dialog>
  );
}

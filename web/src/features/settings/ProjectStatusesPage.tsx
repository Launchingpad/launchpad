import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { ApiError } from '../../api/errors';
import {
  useCreateProjectStatus,
  useDeleteProjectStatus,
  useProjectStatuses,
  useReorderProjectStatuses,
  useUpdateProjectStatus,
} from '../../api/projectStatuses';
import type { ProjectStatus } from '../../api/types';
import ui from '../../styles/ui.module.css';
import { Dialog } from '../../ui/Dialog';
import { EmptyState, SkeletonRows } from '../../ui/States';
import { ProjectStatusIcon } from '../projects/ProjectStatusIcon';
import { FixedError } from './FixedError';
import styles from './products.module.css';
import settings from './settings.module.css';
import { SettingsCard, SettingsPage } from './SettingsParts';

type Kind = ProjectStatus['kind'];

/** The kinds in the order a project moves through them, with the heading each group shows. */
const KINDS: { kind: Kind; heading: string; help: string }[] = [
  { kind: 'backlog', heading: 'Backlog', help: 'Ideas not yet planned.' },
  { kind: 'planned', heading: 'Planned', help: 'Planned, not started.' },
  { kind: 'started', heading: 'Started', help: 'Work is under way. Sets the start time.' },
  { kind: 'paused', heading: 'Paused', help: 'Started, then put on hold.' },
  { kind: 'completed', heading: 'Completed', help: 'Done. Sets the finish time.' },
  { kind: 'canceled', heading: 'Canceled', help: 'Dropped. Sets the cancel time.' },
];
const COLOR_RE = /^#[0-9a-fA-F]{6}$/;
const NAME_RULE =
  'Use 1 to 50 characters, with no comma. The name of another kind (for example Paused) is not allowed.';
const COLOR_RULE = 'Use a colour like #5e6ad2.';

/** Text for a failed status action. The server's 409 texts already say what to do, so they are shown as they are. */
export function statusActionText(err: unknown, what: string): string {
  if (err instanceof ApiError) {
    if (err.status === 403) return `You need admin access to ${what}.`;
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 404) return 'This status is gone. Reload the page.';
    if ((err.status === 409 || err.status === 400) && err.message) return err.message;
  }
  return `Could not ${what}. Try again.`;
}

/** Settings, Project statuses (LAUN-195, decisions-T29). Admin only: SettingsLayout keeps everyone else out. */
export function ProjectStatusesPage() {
  const list = useProjectStatuses();
  const reorder = useReorderProjectStatuses();
  const [dialog, setDialog] = useState<
    { mode: 'add'; kind: Kind } | { mode: 'edit' | 'delete'; status: ProjectStatus } | null
  >(null);
  const [error, setError] = useState<string | null>(null);
  const all = useMemo(() => list.data ?? [], [list.data]);
  // Where focus goes once the list has changed: a data-focus key (`up:<id>`, `down:<id>`, `edit:<id>`, `add:<kind>`).
  const focusKey = useRef<string | null>(null);
  useEffect(() => {
    const key = focusKey.current;
    if (!key) return;
    // After the dialog has returned focus to the control that opened it (a deleted row's button is gone).
    const t = setTimeout(() => {
      const el = document.querySelector<HTMLElement>(`[data-focus="${key}"]`);
      if (el) {
        el.focus();
        focusKey.current = null;
      }
    }, 0);
    return () => clearTimeout(t);
  }, [all]);

  /** Swap a status with its neighbour of the same kind in the org's whole order, then PUT that order. */
  function move(s: ProjectStatus, dir: -1 | 1) {
    const same = all.filter((x) => x.kind === s.kind);
    const at = same.findIndex((x) => x.id === s.id);
    const other = same[at + dir];
    if (!other || reorder.isPending) return; // aria-disabled buttons keep focus, so the click is dropped here
    focusKey.current = `${dir < 0 ? 'up' : 'down'}:${s.id}`;
    const ids = all.map((x) => x.id);
    const a = ids.indexOf(s.id);
    const b = ids.indexOf(other.id);
    [ids[a], ids[b]] = [ids[b]!, ids[a]!];
    setError(null);
    reorder.mutate(ids, {
      onError: (e) => {
        focusKey.current = null;
        setError(statusActionText(e, 'change the order'));
      },
    });
  }

  return (
    <SettingsPage
      title="Project statuses"
      description="Every project has one status. A status belongs to one of six kinds, and the kind decides what the status does (the start, finish and cancel times). Rename a status, change its colour, add more of a kind and put them in order. A status with projects in it, or the only status of its kind, cannot be deleted."
    >
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      {list.isPending ? (
        <SkeletonRows n={4} />
      ) : list.isError ? (
        <FixedError
          error={list.error}
          onRetry={() => void list.refetch()}
          title="Could not load project statuses"
        />
      ) : all.length === 0 ? (
        <EmptyState title="No statuses" />
      ) : (
        KINDS.map(({ kind, heading, help }) => {
          const rows = all.filter((s) => s.kind === kind);
          return (
            <section key={kind} aria-label={heading}>
              <div className={styles.toolbar}>
                <h2 style={{ margin: 0, fontSize: 15 }}>{heading}</h2>
                <span className={ui.hint}>{help}</span>
                <button
                  type="button"
                  className={`${ui.btn} ${ui.ghost}`}
                  aria-label={`Add a ${heading} status`}
                  data-focus={`add:${kind}`}
                  onClick={() => setDialog({ mode: 'add', kind })}
                >
                  Add status
                </button>
              </div>
              <SettingsCard>
                <ul className={styles.list} aria-label={`${heading} statuses`}>
                  {rows.map((s, i) => (
                    <li key={s.id} className={styles.item}>
                      <span className={styles.swatch} aria-hidden="true">
                        <ProjectStatusIcon kind={s.kind} color={s.color} />
                      </span>
                      <div className={styles.text}>
                        <span className={styles.name}>{s.name}</span>
                      </div>
                      <div className={styles.rowActions}>
                        <button
                          type="button"
                          className={`${ui.btn} ${ui.ghost}`}
                          aria-label={`Move ${s.name} up`}
                          data-focus={`up:${s.id}`}
                          aria-disabled={i === 0 || reorder.isPending}
                          onClick={() => move(s, -1)}
                        >
                          Up
                        </button>
                        <button
                          type="button"
                          className={`${ui.btn} ${ui.ghost}`}
                          aria-label={`Move ${s.name} down`}
                          data-focus={`down:${s.id}`}
                          aria-disabled={i === rows.length - 1 || reorder.isPending}
                          onClick={() => move(s, 1)}
                        >
                          Down
                        </button>
                        <button
                          type="button"
                          className={`${ui.btn} ${ui.ghost}`}
                          aria-label={`Edit ${s.name}`}
                          data-focus={`edit:${s.id}`}
                          onClick={() => setDialog({ mode: 'edit', status: s })}
                        >
                          Edit
                        </button>
                        <button
                          type="button"
                          className={`${ui.btn} ${ui.ghost}`}
                          aria-label={`Delete ${s.name}`}
                          onClick={() => setDialog({ mode: 'delete', status: s })}
                        >
                          Delete
                        </button>
                      </div>
                    </li>
                  ))}
                </ul>
              </SettingsCard>
            </section>
          );
        })
      )}
      {dialog?.mode === 'add' && <StatusFormDialog kind={dialog.kind} onClose={() => setDialog(null)} />}
      {dialog?.mode === 'edit' && (
        <StatusFormDialog key={dialog.status.id} status={dialog.status} onClose={() => setDialog(null)} />
      )}
      {dialog?.mode === 'delete' && (
        <DeleteStatusDialog
          status={dialog.status}
          onClose={() => setDialog(null)}
          onDeleted={() => {
            // Focus the next row of the kind (or the previous one, or the kind's Add button).
            const rows = all.filter((x) => x.kind === dialog.status.kind);
            const at = rows.findIndex((x) => x.id === dialog.status.id);
            const next = rows[at + 1] ?? rows[at - 1];
            focusKey.current = next ? `edit:${next.id}` : `add:${dialog.status.kind}`;
          }}
        />
      )}
    </SettingsPage>
  );
}

/** Add (a `kind`) or edit (a `status`): the name and the colour. The kind never changes. */
function StatusFormDialog({
  kind,
  status,
  onClose,
}: {
  kind?: Kind;
  status?: ProjectStatus;
  onClose: () => void;
}) {
  const create = useCreateProjectStatus();
  const update = useUpdateProjectStatus();
  const [name, setName] = useState(status?.name ?? '');
  const [color, setColor] = useState(status?.color ?? '#5e6ad2');
  const [errors, setErrors] = useState<{ name?: string; color?: string }>({});
  const [banner, setBanner] = useState<string | null>(null);
  const pending = create.isPending || update.isPending;

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBanner(null);
    const n = name.trim();
    const c = color.trim();
    const found: typeof errors = {};
    if (n.length < 1 || [...n].length > 50 || n.includes(',')) found.name = NAME_RULE;
    if (!COLOR_RE.test(c)) found.color = COLOR_RULE;
    setErrors(found);
    if (found.name || found.color) {
      document.getElementById(found.name ? 'status-name' : 'status-color')?.focus();
      return;
    }
    try {
      if (status) {
        const body: { name?: string; color?: string } = {};
        if (n !== status.name) body.name = n;
        if (c.toLowerCase() !== status.color) body.color = c;
        if (Object.keys(body).length > 0) await update.mutateAsync({ id: status.id, body });
      } else {
        await create.mutateAsync({ kind: kind!, name: n, color: c });
      }
      onClose();
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setErrors({ name: err.message });
        document.getElementById('status-name')?.focus();
      } else if (err instanceof ApiError && err.status === 400 && /^name/i.test(err.message)) {
        setErrors({ name: err.message });
        document.getElementById('status-name')?.focus();
      } else setBanner(statusActionText(err, status ? 'save the status' : 'add the status'));
    }
  }

  return (
    <Dialog title={status ? `Edit ${status.name}` : 'Add a status'} width={460} onClose={onClose}>
      <form onSubmit={(e) => void submit(e)} noValidate>
        {banner && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            {banner}
          </p>
        )}
        <div className={ui.field}>
          <label htmlFor="status-name">Name</label>
          <input
            id="status-name"
            className={ui.input}
            value={name}
            onChange={(e) => setName(e.target.value)}
            aria-invalid={!!errors.name}
            aria-describedby={errors.name ? 'status-name-error' : undefined}
            autoComplete="off"
            data-autofocus
          />
          {errors.name && (
            <span id="status-name-error" className={settings.fieldError} aria-live="polite">
              {errors.name}
            </span>
          )}
        </div>
        <div className={ui.field}>
          <label htmlFor="status-color">Colour</label>
          <div className={styles.colorRow}>
            <input
              id="status-color"
              className={ui.input}
              value={color}
              onChange={(e) => setColor(e.target.value)}
              aria-invalid={!!errors.color}
              aria-describedby={errors.color ? 'status-color-error' : undefined}
              spellCheck={false}
              autoComplete="off"
            />
            {COLOR_RE.test(color.trim()) && (
              <span className={styles.preview} style={{ background: color.trim() }} aria-hidden="true" />
            )}
          </div>
          {errors.color && (
            <span id="status-color-error" className={settings.fieldError} aria-live="polite">
              {errors.color}
            </span>
          )}
        </div>
        <div className={styles.footer}>
          <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={pending}>
            {pending ? 'Saving…' : status ? 'Save' : 'Add status'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function DeleteStatusDialog({
  status,
  onClose,
  onDeleted,
}: {
  status: ProjectStatus;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const del = useDeleteProjectStatus();
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog title={`Delete ${status.name}?`} width={460} onClose={onClose}>
      <p>
        This deletes the status. It works only when no project has it and another status of the same kind
        stays.
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
            del.mutate(status.id, {
              onSuccess: () => {
                onDeleted();
                onClose();
              },
              onError: (e) => setError(statusActionText(e, 'delete the status')),
            })
          }
        >
          {del.isPending ? 'Deleting…' : 'Delete status'}
        </button>
      </div>
    </Dialog>
  );
}

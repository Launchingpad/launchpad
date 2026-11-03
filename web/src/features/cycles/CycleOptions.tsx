import { useRef, useState, type FormEvent } from 'react';
import { useHref, useNavigate } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import type { Cycle } from '../../api/types';
import { uuid } from '../../lib/uuid';
import { useIsOrgAdmin } from '../settings/access';
import { Dialog } from '../../ui/Dialog';
import { IconMore } from '../../ui/Icons';
import { Menu, type MenuItem } from '../../ui/Menu';
import { useToast } from '../../ui/Toast';
import ui from '../../styles/ui.module.css';
import { cycleDays, cycleErrorText, dayAfter, optionReasons } from './cycleRules';
import { cycleKind, cycleName } from './cycles';
import { useCompleteCycle, useDeleteCycle, useUpdateCycle } from './queries';
import styles from './cycles.module.css';

type Dialogs = 'edit' | 'dates' | 'complete' | 'delete' | null;

/**
 * The `...` menu of the cycle page (LAUN-172, decisions-T27): edit name and description, change dates, mark
 * complete, delete, copy link. A row that a rule forbids stays, dimmed, with its reason; the server judges again.
 * Mark complete and Delete need the admin scope: for a member they are disabled, not hidden.
 */
export function CycleOptions({
  cycle,
  next,
  zone,
  teamKey,
  rollover,
  now,
}: {
  cycle: Cycle;
  /** The cycle that starts after this one. */
  next: Cycle | null;
  zone: string | undefined;
  teamKey: string;
  rollover: boolean;
  now: number;
}) {
  const org = useOrgSlug();
  const toast = useToast();
  const [open, setOpen] = useState<Dialogs>(null);
  const href = useHref(`/${org}/team/${teamKey}/cycle/${cycle.number}`);
  const isAdmin = useIsOrgAdmin();
  const why = optionReasons(cycle, next, now, isAdmin);

  async function copy() {
    const link = new URL(href, window.location.origin).toString();
    try {
      await navigator.clipboard.writeText(link);
      toast.show('Link copied');
    } catch {
      toast.show('Could not copy the link', { kind: 'error' });
    }
  }

  const items: MenuItem[] = [
    { label: 'Edit name and description…', onSelect: () => setOpen('edit') },
    { label: 'Change dates…', disabled: !!why.dates, reason: why.dates, onSelect: () => setOpen('dates') },
    {
      label: 'Mark complete…',
      disabled: !!why.complete,
      reason: why.complete,
      onSelect: () => setOpen('complete'),
    },
    {
      label: 'Delete cycle…',
      danger: true,
      disabled: !!why.remove,
      reason: why.remove,
      onSelect: () => setOpen('delete'),
    },
    { label: 'Copy link', onSelect: () => void copy() },
  ];
  const close = () => setOpen(null);
  return (
    <>
      <Menu
        closeKey={cycle.id}
        label="Cycle options"
        trigger={<IconMore />}
        hideChevron
        triggerClassName={styles.options}
        wrapClassName={styles.optionsWrap}
        panelClassName={styles.optionsPanel}
        items={items}
      />
      {open === 'edit' && <EditDialog cycle={cycle} onClose={close} />}
      {open === 'dates' && <DatesDialog cycle={cycle} zone={zone} now={now} onClose={close} />}
      {open === 'complete' && (
        <CompleteDialog cycle={cycle} next={next} rollover={rollover} onClose={close} />
      )}
      {open === 'delete' && <DeleteDialog cycle={cycle} teamKey={teamKey} onClose={close} />}
    </>
  );
}

function Banner({ text }: { text: string | null }) {
  return text ? (
    <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
      {text}
    </p>
  ) : null;
}

function EditDialog({ cycle, onClose }: { cycle: Cycle; onClose: () => void }) {
  const update = useUpdateCycle(cycle.id);
  const [name, setName] = useState(cycle.name ?? '');
  const [description, setDescription] = useState(cycle.description);
  const [error, setError] = useState<string | null>(null);
  function submit(e: FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    const body: { name?: string | null; description?: string } = {};
    // An empty name clears it: the cycle shows as "Cycle N" again.
    if (trimmed !== (cycle.name ?? '')) body.name = trimmed === '' ? null : trimmed;
    if (description !== cycle.description) body.description = description;
    if (Object.keys(body).length === 0) return onClose();
    update.mutate(body, {
      onSuccess: onClose,
      onError: (err) => setError(cycleErrorText(err, 'save the cycle')),
    });
  }
  return (
    <Dialog title={`Edit ${cycleName(cycle)}`} width={480} onClose={onClose}>
      <form onSubmit={submit} noValidate>
        <div className={ui.field}>
          <label htmlFor="cycle-name">Name</label>
          <input
            id="cycle-name"
            className={ui.input}
            value={name}
            maxLength={255}
            placeholder={`Cycle ${cycle.number}`}
            onChange={(e) => setName(e.target.value)}
            autoComplete="off"
            data-autofocus
          />
        </div>
        <div className={ui.field}>
          <label htmlFor="cycle-description">Description</label>
          <textarea
            id="cycle-description"
            className={ui.textarea}
            rows={4}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>
        <Banner text={error} />
        <div className={styles.dialogFooter}>
          <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={update.isPending}>
            {update.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function DatesDialog({
  cycle,
  zone,
  now,
  onClose,
}: {
  cycle: Cycle;
  zone: string | undefined;
  now: number;
  onClose: () => void;
}) {
  const update = useUpdateCycle(cycle.id);
  const days = cycleDays(cycle, zone);
  const [first, setFirst] = useState(days?.first ?? '');
  const [last, setLast] = useState(days?.last ?? '');
  const [error, setError] = useState<string | null>(null);
  const started = cycleKind(cycle, now) === 'current';
  const changed = days
    ? { first: first !== days.first, last: last !== days.last }
    : { first: false, last: false };
  function submit(e: FormEvent) {
    e.preventDefault();
    if (!first || !last) return setError('Choose both days.');
    if (last < first) return setError('The last day cannot be before the first day.');
    // Only what changed goes out, so an untouched day keeps its stored time (an imported cycle may differ from the cadence).
    const body: { startsAt?: string; endsAt?: string } = {};
    if (changed.first && !started) body.startsAt = first;
    if (changed.last) body.endsAt = days?.exclusiveEnd === false ? last : dayAfter(last);
    if (!body.startsAt && !body.endsAt) return onClose();
    update.mutate(body, {
      onSuccess: onClose,
      onError: (err) => setError(cycleErrorText(err, 'change the dates')),
    });
  }
  return (
    <Dialog title={`Change dates of ${cycleName(cycle)}`} width={440} onClose={onClose}>
      <form onSubmit={submit} noValidate>
        <p className={ui.hint}>
          The next cycle starts the day after the last day, so the cycles stay in a row.
          {started && ' This cycle has started, so only its last day can change.'}
        </p>
        <div className={ui.field}>
          <label htmlFor="cycle-first">First day</label>
          <input
            id="cycle-first"
            type="date"
            className={ui.input}
            value={first}
            disabled={started}
            onChange={(e) => setFirst(e.target.value)}
          />
        </div>
        <div className={ui.field}>
          <label htmlFor="cycle-last">Last day</label>
          <input
            id="cycle-last"
            type="date"
            className={ui.input}
            value={last}
            onChange={(e) => setLast(e.target.value)}
            data-autofocus
          />
        </div>
        <Banner text={error} />
        <div className={styles.dialogFooter}>
          <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={update.isPending}>
            {update.isPending ? 'Saving…' : 'Save dates'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function CompleteDialog({
  cycle,
  next,
  rollover,
  onClose,
}: {
  cycle: Cycle;
  next: Cycle | null;
  rollover: boolean;
  onClose: () => void;
}) {
  const complete = useCompleteCycle(cycle.id);
  const key = useRef(uuid());
  const [error, setError] = useState<string | null>(null);
  const nextName = next ? cycleName(next) : 'the next cycle';
  return (
    <Dialog title={`Complete ${cycleName(cycle)}?`} width={460} onClose={onClose}>
      <p>
        The cycle ends now and {nextName} starts now.{' '}
        {rollover
          ? `Issues that are not done or canceled move to ${nextName}.`
          : 'Its issues stay in this cycle (rollover is off for the team).'}
      </p>
      <Banner text={error} />
      <div className={styles.dialogFooter}>
        <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose} data-autofocus>
          Cancel
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.primary}`}
          disabled={complete.isPending}
          onClick={() =>
            complete.mutate(key.current, {
              onSuccess: onClose,
              onError: (err) => {
                // A refusal is final for this key; a new try needs a new one.
                key.current = uuid();
                setError(cycleErrorText(err, 'complete the cycle'));
              },
            })
          }
        >
          {complete.isPending ? 'Completing…' : 'Complete cycle'}
        </button>
      </div>
    </Dialog>
  );
}

function DeleteDialog({ cycle, teamKey, onClose }: { cycle: Cycle; teamKey: string; onClose: () => void }) {
  const org = useOrgSlug();
  const nav = useNavigate();
  const del = useDeleteCycle(cycle.id);
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog title={`Delete ${cycleName(cycle)}?`} width={460} onClose={onClose}>
      <p>
        This deletes the cycle for good, with its burn-up history. The next cycle takes over its dates. A
        cycle that still has issues cannot be deleted: move them first.
      </p>
      <Banner text={error} />
      <div className={styles.dialogFooter}>
        <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose} data-autofocus>
          Cancel
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.danger}`}
          disabled={del.isPending}
          onClick={() =>
            del.mutate(undefined, {
              onSuccess: () => nav(`/${org}/team/${teamKey}/cycles`),
              onError: (err) => setError(cycleErrorText(err, 'delete the cycle')),
            })
          }
        >
          {del.isPending ? 'Deleting…' : 'Delete cycle'}
        </button>
      </div>
    </Dialog>
  );
}

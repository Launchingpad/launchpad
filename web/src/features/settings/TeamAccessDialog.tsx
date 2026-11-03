import { useEffect, useId, useRef, useState, type FormEvent, type MouseEvent } from 'react';
import { ApiError } from '../../api/errors';
import { useSetTeamAccess, useTeams } from '../../api/queries';
import type { Member, TeamAccessInput, TeamAccessResult } from '../../api/types';
import ui from '../../styles/ui.module.css';
import { Dialog } from '../../ui/Dialog';
import styles from './settings.module.css';
import { TeamChecklist } from './TeamChecklist';
import { accessErrorText, plural, previewSummary, savedNote } from './teamAccessText';

type Mode = TeamAccessInput['teamAccess'];
type Choice = NonNullable<TeamAccessInput['assignedIssues']>;

/**
 * Edit a member's team access (decisions-T20 #74, LAUN-227). The admin picks "All teams" or a checklist, then
 * "Check changes" sends a dry run (#48) and shows the counts. When the member has open issues in a team they
 * lose, the admin picks what happens to them; "Unassign" is the default (#49, open point E). Any edit after the
 * check needs a new check before Save, and Save exists only after the counts were drawn.
 */
export function TeamAccessDialog({
  member,
  onClose,
  onSaved,
}: {
  member: Member;
  onClose: () => void;
  /** Called with the note for the page, only while this dialog is still open. */
  onSaved: (note: string) => void;
}) {
  const id = useId();
  const teams = useTeams();
  const set = useSetTeamAccess();
  const startSelected = member.teamAccess === 'selected';
  const [mode, setMode] = useState<Mode>(startSelected ? 'selected' : 'all');
  const [picked, setPicked] = useState<string[]>(startSelected ? (member.teams ?? []) : []);
  const [preview, setPreview] = useState<{ sig: string; result: TeamAccessResult } | null>(null);
  const [choice, setChoice] = useState<Choice>('unassign');
  const [error, setError] = useState<string | null>(null);
  const [announce, setAnnounce] = useState('');
  const headingRef = useRef<HTMLHeadingElement>(null);
  // A request in flight; the mutation's own flag updates one render late, so a fast second click or Enter could
  // pass it.
  const busy = useRef(false);
  // A save that answers after the dialog closed must not touch the page (another member's dialog may be open).
  const open = useRef(true);
  useEffect(() => {
    open.current = true;
    return () => {
      open.current = false;
    };
  }, []);

  const body: TeamAccessInput =
    mode === 'all' ? { teamAccess: 'all' } : { teamAccess: 'selected', teams: [...picked].sort() };
  const sig = JSON.stringify(body);
  const result = preview && preview.sig === sig ? preview.result : null;
  const assignedTotal = result ? result.assignedInLostTeams.reduce((n, t) => n + t.openIssues, 0) : 0;
  // The dry run is sent without a choice, so its hiddenAssigned still holds the issues an Unassign clears.
  const hiddenAfter = result
    ? choice === 'unassign'
      ? Math.max(0, result.hiddenAssigned - assignedTotal)
      : result.hiddenAssigned
    : 0;
  const noTeam = mode === 'selected' && picked.length === 0;

  const listed = teams.data ?? [];
  const keyOf = new Map(listed.map((t) => [t.id, t.key]));
  // A team the member has that the list does not show stays in the checklist (from the member's row, so it can be
  // unticked and ticked again): a save never drops it without the admin seeing it.
  const extra = startSelected ? (member.teams ?? []) : [];

  useEffect(() => {
    if (preview) headingRef.current?.focus();
  }, [preview]);

  function edit(fn: () => void) {
    setError(null);
    setAnnounce('');
    fn();
  }

  function teamNames(ids: string[]): string {
    const names = ids.flatMap((i) => (keyOf.has(i) ? [keyOf.get(i)!] : []));
    const unknown = ids.length - names.length;
    if (unknown > 0)
      names.push(unknown === 1 ? '1 team not in the list' : `${unknown} teams not in the list`);
    return names.length === 0 ? 'None' : names.join(', ');
  }

  async function check(e?: FormEvent) {
    e?.preventDefault();
    if (busy.current || !teams.isSuccess) return;
    busy.current = true;
    setError(null);
    setAnnounce('');
    try {
      const r = await set.mutateAsync({ user: member.id, body, dryRun: true });
      if (!open.current) return;
      setPreview({ sig, result: r });
      setAnnounce(previewSummary(r, noTeam));
    } catch (err) {
      if (!open.current) return;
      setPreview(null);
      setError(accessErrorText(err));
    } finally {
      busy.current = false;
    }
  }

  async function save(e: MouseEvent) {
    // A double-click on "Check changes" must never land its second click here.
    if (e.detail > 1 || busy.current || !result || !result.changed) return;
    busy.current = true;
    setError(null);
    const asked = assignedTotal > 0 ? choice : undefined;
    try {
      const r = await set.mutateAsync({
        user: member.id,
        body: asked ? { ...body, assignedIssues: asked } : body,
        dryRun: false,
      });
      if (!open.current) return;
      onSaved(savedNote(member.name, result, r, asked));
    } catch (err) {
      if (!open.current) return;
      // A 409 means the state moved after the check (a new assignment, a repository without a team): check again.
      if (err instanceof ApiError && err.status === 409) setPreview(null);
      setError(accessErrorText(err));
    } finally {
      busy.current = false;
    }
  }

  const pending = set.isPending;
  const footer = (
    <>
      <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose}>
        Cancel
      </button>
      {result ? (
        <button
          key="save"
          type="button"
          className={`${ui.btn} ${ui.primary}`}
          disabled={!result.changed}
          aria-disabled={pending || undefined}
          onClick={(e) => void save(e)}
        >
          {pending ? 'Saving…' : 'Save access'}
        </button>
      ) : (
        <button
          key="check"
          type="submit"
          form={`${id}-form`}
          className={`${ui.btn} ${ui.primary}`}
          disabled={!teams.isSuccess}
          aria-disabled={pending || undefined}
        >
          {pending ? 'Checking…' : 'Check changes'}
        </button>
      )}
    </>
  );

  return (
    <Dialog title={`Team access for ${member.name}`} width={520} onClose={onClose} footer={footer}>
      <form id={`${id}-form`} onSubmit={(e) => void check(e)} noValidate>
        <fieldset className={styles.radios}>
          <legend>Team access</legend>
          <label>
            <input
              type="radio"
              name={`${id}-mode`}
              checked={mode === 'all'}
              onChange={() => edit(() => setMode('all'))}
              data-autofocus={startSelected ? undefined : true}
            />
            <span>All teams</span>
          </label>
          <label>
            <input
              type="radio"
              name={`${id}-mode`}
              checked={mode === 'selected'}
              onChange={() => edit(() => setMode('selected'))}
              data-autofocus={startSelected ? true : undefined}
            />
            <span>Selected teams</span>
          </label>
        </fieldset>

        {teams.isPending ? (
          <p role="status" className={ui.hint}>
            Loading teams…
          </p>
        ) : teams.isError ? (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            Could not load the teams. You can check the changes when the list loads.{' '}
            <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={() => void teams.refetch()}>
              Try again
            </button>
          </p>
        ) : (
          mode === 'selected' && (
            <>
              <TeamChecklist
                teams={listed}
                picked={picked}
                extra={extra}
                onChange={(next) => edit(() => setPicked(next))}
              />
              {noTeam && (
                <p className={ui.hint}>With no team ticked, this person sees no team and no issue.</p>
              )}
            </>
          )
        )}
      </form>

      {result && (
        <section aria-labelledby={`${id}-changes`} data-testid="access-preview">
          <h3 id={`${id}-changes`} ref={headingRef} tabIndex={-1} className={styles.previewTitle}>
            Changes
          </h3>
          {!result.changed ? (
            <p className={ui.hint}>Nothing changes. This is the access {member.name} has now.</p>
          ) : (
            <>
              {noTeam && (
                <p className={`${ui.banner}`}>{member.name} will see no team and no issue after the save.</p>
              )}
              <dl className={`${styles.facts} ${styles.factsWrap}`}>
                <dt>Teams added</dt>
                <dd>{teamNames(result.addedTeamIds)}</dd>
                <dt>Teams removed</dt>
                <dd>{teamNames(result.lostTeamIds)}</dd>
                <dt>Issue subscriptions removed</dt>
                <dd>{result.removed.subscriptions}</dd>
                <dt>Team follows removed</dt>
                <dd>{result.removed.follows}</dd>
                <dt>Team notification settings removed</dt>
                <dd>{result.removed.settings}</dd>
                <dt>After the save: open issues assigned to them in teams they cannot see</dt>
                <dd>{hiddenAfter}</dd>
              </dl>
            </>
          )}
          {assignedTotal > 0 && (
            <fieldset className={styles.radios}>
              <legend>
                {`${plural(assignedTotal, 'open issue')} assigned to ${member.name} in the teams removed (${result.assignedInLostTeams
                  .map((t) => `${keyOf.get(t.teamId) ?? 'a team not in the list'}: ${t.openIssues}`)
                  .join(', ')})`}
              </legend>
              <label>
                <input
                  type="radio"
                  name={`${id}-assigned`}
                  checked={choice === 'unassign'}
                  onChange={() => setChoice('unassign')}
                />
                <span>Unassign them (recommended)</span>
              </label>
              <label>
                <input
                  type="radio"
                  name={`${id}-assigned`}
                  checked={choice === 'keep'}
                  onChange={() => setChoice('keep')}
                />
                <span>Keep them assigned. {member.name} will not see them.</span>
              </label>
            </fieldset>
          )}
        </section>
      )}

      <p role="status" className="sr-only" data-testid="access-announce">
        {announce}
      </p>
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
    </Dialog>
  );
}

import { useMemo, useState, type FormEvent } from 'react';
import { ApiError } from '../../../api/errors';
import { useGitWorkflow, useSaveGitWorkflow, useTeams, useTeamStates } from '../../../api/queries';
import type {
  GitWorkflow,
  GitWorkflowEvent,
  GitWorkflowRuleInput,
  Team,
  WorkflowState,
} from '../../../api/types';
import ui from '../../../styles/ui.module.css';
import { EmptyState, Spinner } from '../../../ui/States';
import { FixedError } from '../FixedError';
import { SettingsCard, SettingsPage, SettingsRow, SettingsSection } from '../SettingsParts';
import org from '../organisation.module.css';
import styles from './gitWorkflow.module.css';

/**
 * Settings, Git workflow (LAUN-244, decisions-T44): for each team, the state a linked issue moves to when a
 * merge or pull request opens, is marked ready, reopens, merges or closes without a merge. Admin only:
 * SettingsLayout keeps everyone else out.
 */

export const GIT_EVENTS: { event: GitWorkflowEvent; label: string; help: string }[] = [
  { event: 'opened', label: 'Opened', help: 'A merge or pull request opens. A draft never moves the issue.' },
  { event: 'ready', label: 'Ready for review', help: 'A draft is marked ready. GitLab and GitHub only.' },
  { event: 'reopened', label: 'Reopened', help: 'A closed request opens again. GitLab and GitHub only.' },
  { event: 'merged', label: 'Merged', help: 'The request is merged.' },
  { event: 'closed', label: 'Closed without merge', help: 'The request is closed and not merged.' },
];

/** The value of a picker: the built-in move, no move, or a state id. */
const BUILTIN = 'builtin';
const NONE = 'none';

type Choices = Record<GitWorkflowEvent, string>;

/** A fixed sentence for a failed save. No server text reaches the screen. */
export function gitWorkflowErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400:
      return 'The server did not accept these rules. A state may be archived or removed. Reload the page and choose again.';
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'You need admin access to change the git workflow.';
    case 404:
      return 'The server cannot find the team. Reload the page and try again.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    default:
      return 'Could not save the git workflow. Try again.';
  }
}

/** The built-in target of an event: the team's review or testing state, or none (closed). */
export function builtinTarget(team: Team, event: GitWorkflowEvent): string | null {
  if (event === 'merged') return team.testingStateId;
  if (event === 'closed') return null;
  return team.reviewStateId;
}

function choicesOf(wf: GitWorkflow): Choices {
  const out = {} as Choices;
  for (const { event } of GIT_EVENTS) {
    const r = wf.rules.find((x) => x.event === event);
    out[event] = !r || !r.custom ? BUILTIN : (r.stateId ?? NONE);
  }
  return out;
}

/** The state an event goes to under a choice, as a name: "no move" when it goes nowhere. */
export function targetName(choice: string, builtin: string | null, names: Map<string, string>): string {
  const id = choice === BUILTIN ? builtin : choice === NONE ? null : choice;
  if (!id) return 'no move';
  return names.get(id) ?? 'an archived state';
}

/** The one-line preview: "Opened → In Review · ... · Closed without merge → no move". */
export function previewLine(team: Team, choices: Choices, names: Map<string, string>): string {
  return GIT_EVENTS.map(
    ({ event, label }) => `${label} → ${targetName(choices[event], builtinTarget(team, event), names)}`,
  ).join(' · ');
}

export function GitWorkflowPage() {
  const teams = useTeams();
  return (
    <SettingsPage
      title="Git workflow"
      description="When a linked merge request or pull request changes, Launchpad moves the issue it names in its branch or title. Choose the state for each event, for each team. A built-in move goes forward only and never moves an issue out of a completed or canceled state."
    >
      <div className={org.touch}>
        {teams.isPending ? (
          <Spinner label="Loading the teams" />
        ) : teams.isError ? (
          <FixedError
            title="Could not load the teams"
            error={teams.error}
            onRetry={() => void teams.refetch()}
          />
        ) : teams.data.length === 0 ? (
          <EmptyState title="No teams">Create a team first.</EmptyState>
        ) : (
          teams.data.map((t) => <TeamWorkflow key={t.id} team={t} />)
        )}
      </div>
    </SettingsPage>
  );
}

function TeamWorkflow({ team }: { team: Team }) {
  const wf = useGitWorkflow(team.key);
  const states = useTeamStates(team.key);
  // Kept here, not in the form: a save gives a new server answer, which starts the form again.
  const [ok, setOk] = useState('');
  return (
    <SettingsSection title={`${team.name} (${team.key})`}>
      {wf.isPending || states.isPending ? (
        <Spinner label={`Loading the git workflow of ${team.key}`} />
      ) : wf.isError ? (
        <FixedError
          title="Could not load the git workflow"
          error={wf.error}
          onRetry={() => void wf.refetch()}
        />
      ) : states.isError ? (
        <FixedError
          title="Could not load the states"
          error={states.error}
          onRetry={() => void states.refetch()}
        />
      ) : (
        // A new server answer (a save here, or another admin's) starts the form again.
        <RulesForm
          key={JSON.stringify(wf.data.rules)}
          team={team}
          wf={wf.data}
          states={states.data}
          ok={ok}
          setOk={setOk}
        />
      )}
    </SettingsSection>
  );
}

function RulesForm({
  team,
  wf,
  states,
  ok,
  setOk,
}: {
  team: Team;
  wf: GitWorkflow;
  states: WorkflowState[];
  ok: string;
  setOk: (s: string) => void;
}) {
  const save = useSaveGitWorkflow(team.key);
  const saved = useMemo(() => choicesOf(wf), [wf]);
  const [choices, setChoices] = useState<Choices>(saved);
  const [banner, setBanner] = useState<string | null>(null);
  const names = useMemo(() => new Map(states.map((s) => [s.id, s.name])), [states]);
  const custom = wf.rules.some((r) => r.custom);
  /** The "only from" set (API only) of a rule whose target is unchanged; a new target or no move drops it. */
  const fromOf = (event: GitWorkflowEvent) => {
    const r = wf.rules.find((x) => x.event === event);
    return r?.custom && r.stateId === choices[event] ? r.fromStateIds : [];
  };
  /** True when the kept set names a state that is archived since the rule was saved: a save leaves it out. */
  const staleFrom = (event: GitWorkflowEvent) => fromOf(event).some((f) => !names.has(f));
  const dirty = GIT_EVENTS.some(({ event }) => choices[event] !== saved[event] || staleFrom(event));

  /** A row whose chosen state is not a live state of the team: archived since the rule was saved. */
  const archived = (event: GitWorkflowEvent) => {
    const c = choices[event];
    if (c === NONE) return false;
    const id = c === BUILTIN ? builtinTarget(team, event) : c;
    return id !== null && !names.has(id);
  };
  const stale = GIT_EVENTS.filter(({ event }) => archived(event));

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (save.isPending) return;
    setBanner(null);
    setOk('');
    const custom = stale.filter(({ event }) => choices[event] !== BUILTIN);
    if (custom.length > 0) {
      setBanner(`Choose another state for ${custom.map((s) => s.label).join(', ')}: its state is archived.`);
      return;
    }
    if (!dirty) {
      setBanner('Nothing changed.');
      return;
    }
    const rules: GitWorkflowRuleInput[] = [];
    for (const { event } of GIT_EVENTS) {
      const c = choices[event];
      if (c === BUILTIN) continue;
      // Archived "only from" states are left out: the server refuses them.
      const keepFrom = fromOf(event).filter((f) => names.has(f));
      rules.push({ event, stateId: c === NONE ? null : c, fromStateIds: keepFrom });
    }
    try {
      await save.mutateAsync(rules);
      setOk('Saved.');
    } catch (err) {
      setBanner(gitWorkflowErrorText(err));
    }
  }

  async function reset() {
    if (save.isPending || !custom) return;
    setBanner(null);
    setOk('');
    try {
      await save.mutateAsync([]);
      setOk('Reset to defaults.');
    } catch (err) {
      setBanner(gitWorkflowErrorText(err));
    }
  }

  const id = (event: string) => `gw-${team.key}-${event}`;
  return (
    <SettingsCard>
      <form onSubmit={(e) => void submit(e)} noValidate aria-label={`${team.key} git workflow`}>
        {GIT_EVENTS.map(({ event, label, help }) => {
          const c = choices[event];
          const builtin = builtinTarget(team, event);
          const from = fromOf(event).filter((f) => names.has(f));
          const fromGone = staleFrom(event);
          const gone = archived(event);
          const problems = [gone && `${id(event)}-problem`, fromGone && `${id(event)}-from-problem`]
            .filter(Boolean)
            .join(' ');
          return (
            <SettingsRow
              key={event}
              label={label}
              labelFor={id(event)}
              description={
                <>
                  {help}
                  {from.length > 0 && (
                    <span className={styles.note}>
                      {' '}
                      Only from: {from.map((f) => names.get(f)).join(', ')}.
                    </span>
                  )}
                  {fromGone && (
                    <span className={org.fieldError} id={`${id(event)}-from-problem`}>
                      {' '}
                      An &ldquo;only from&rdquo; state of this move is archived. Save leaves it out
                      {from.length === 0 ? '; with none left, the move goes forward only' : ''}.
                    </span>
                  )}
                  {gone && (
                    <span className={org.fieldError} id={`${id(event)}-problem`}>
                      {' '}
                      The state of this move is archived. It moves nothing. Choose another state.
                    </span>
                  )}
                </>
              }
            >
              <select
                id={id(event)}
                className={`${ui.select} ${styles.picker}`}
                value={c}
                aria-invalid={gone || fromGone || undefined}
                aria-describedby={problems || undefined}
                onChange={(e) => {
                  setChoices({ ...choices, [event]: e.target.value });
                  setBanner(null);
                  setOk('');
                }}
              >
                <option value={BUILTIN}>Default: {targetName(BUILTIN, builtin, names)}</option>
                {c !== BUILTIN && c !== NONE && !names.has(c) && <option value={c}>Archived state</option>}
                {states.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
                <option value={NONE}>No move</option>
              </select>
            </SettingsRow>
          );
        })}
        <div className={styles.footer}>
          <p className={styles.preview} aria-label={`${team.key} preview`}>
            {previewLine(team, choices, names)}
          </p>
          {banner && (
            <p role="alert" className={org.problem}>
              {banner}
            </p>
          )}
          <div className={org.actions}>
            <button
              type="submit"
              className={`${ui.btn} ${ui.primary}`}
              aria-disabled={save.isPending || undefined}
            >
              {save.isPending ? 'Saving' : 'Save'}
            </button>
            <button
              type="button"
              className={ui.btn}
              aria-disabled={save.isPending || !custom || undefined}
              onClick={() => void reset()}
            >
              Reset to defaults
            </button>
            <p role="status" className={org.ok}>
              {ok}
            </p>
          </div>
        </div>
      </form>
    </SettingsCard>
  );
}

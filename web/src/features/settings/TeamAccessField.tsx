import { ApiError } from '../../api/errors';
import { useTeams } from '../../api/queries';
import styles from './settings.module.css';
import { TeamChecklist } from './TeamChecklist';
import ui from '../../styles/ui.module.css';

/** The team access of a new agent and of a sign-up approval: decisions-T20 #29, #31, #75. */
export interface TeamAccessValue {
  teamAccess: 'all' | 'selected';
  /** Team keys. Used only with `selected`. */
  teams: string[];
}

export const allTeamsAccess: TeamAccessValue = { teamAccess: 'all', teams: [] };

export const PICK_A_TEAM = 'Pick at least one team, or choose All teams.';
export const TEAM_GONE = 'A team you picked is gone. Pick the teams again.';
export const TOO_MANY = 'Pick at most 50 teams, or choose All teams.';
export const GUARD =
  'Every repository of an enabled git integration needs a team before anyone can see only selected teams. Give each repository a team in Integrations, or choose All teams.';

/** The two texts of the webhook guard (server/internal/tracker/webhook_guard.go): the start of each, nothing else. */
const isGuard = (m: string) =>
  m.startsWith('some members see only selected teams') || m.startsWith('this cannot be done now');

/**
 * A refusal that belongs to the team access control, as a fixed text (the guard's own text names repositories:
 * never shown). `gone` means a picked team no longer exists: the caller reads the teams again and clears the
 * ticks. Anything else is null: the caller maps it as before.
 */
export function teamAccessError(err: unknown): { text: string; gone?: true } | null {
  if (!(err instanceof ApiError)) return null;
  if (err.status === 409 && isGuard(err.message)) return { text: GUARD };
  if (err.status !== 400) return null;
  if (err.message.startsWith('at most')) return { text: TOO_MANY };
  if (err.message.startsWith('teamAccess "selected" needs')) return { text: PICK_A_TEAM };
  if (err.message.startsWith('unknown team')) return { text: TEAM_GONE, gone: true };
  return null;
}

/** The body the API takes: `teams` only with `selected` (the server refuses it with `all`). */
export function accessBody(
  v: TeamAccessValue,
): { teamAccess: 'all' } | { teamAccess: 'selected'; teams: string[] } {
  return v.teamAccess === 'all' ? { teamAccess: 'all' } : { teamAccess: 'selected', teams: v.teams };
}

/**
 * "All teams" (the default, open point A) or "Selected teams" with a checklist of the org's teams. The caller
 * holds the value and checks that `selected` has a team (PICK_A_TEAM); the server stays the judge.
 */
export function TeamAccessField({
  idPrefix,
  value,
  onChange,
  error,
}: {
  idPrefix: string;
  value: TeamAccessValue;
  onChange: (v: TeamAccessValue) => void;
  error?: string;
}) {
  const teams = useTeams();
  const id = (s: string) => `${idPrefix}-${s}`;
  return (
    <fieldset
      className={styles.radios}
      id={id('teams')}
      aria-describedby={error ? id('teams-error') : id('teams-hint')}
    >
      <legend>Team access</legend>
      <label>
        <input
          type="radio"
          name={id('access')}
          checked={value.teamAccess === 'all'}
          onChange={() => onChange({ teamAccess: 'all', teams: value.teams })}
        />
        <span>All teams</span>
      </label>
      <label>
        <input
          type="radio"
          name={id('access')}
          checked={value.teamAccess === 'selected'}
          onChange={() => onChange({ teamAccess: 'selected', teams: value.teams })}
        />
        <span>Selected teams</span>
      </label>
      {value.teamAccess === 'selected' &&
        (teams.isPending ? (
          <p role="status" className={ui.hint}>
            Loading teams…
          </p>
        ) : teams.isError ? (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            Could not load the teams.{' '}
            <button type="button" className={ui.btn} onClick={() => void teams.refetch()}>
              Try again
            </button>
          </p>
        ) : teams.data.length === 0 ? (
          <p className={ui.hint}>The organisation has no teams yet. Create a team first.</p>
        ) : (
          // The one checklist of the members dialog and the invite form (LAUN-227), with its product shortcuts.
          // Focus lands on its first box on an error: the box itself names it.
          <div className={styles.teamChecks}>
            <TeamChecklist
              teams={teams.data}
              picked={value.teams}
              onChange={(next) => onChange({ teamAccess: 'selected', teams: next })}
              invalid={!!error}
              describedBy={error ? id('teams-error') : undefined}
            />
          </div>
        ))}
      {error ? (
        <span id={id('teams-error')} className={styles.fieldError} aria-live="polite">
          {error}
        </span>
      ) : (
        <span id={id('teams-hint')} className={styles.hint}>
          {value.teamAccess === 'all'
            ? 'Sees every team, also teams made later.'
            : 'Sees only the teams ticked here. Its tokens see the same.'}
        </span>
      )}
    </fieldset>
  );
}

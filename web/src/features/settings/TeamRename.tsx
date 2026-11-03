import { useState, type FormEvent } from 'react';
import { ApiError } from '../../api/errors';
import { useRenameTeam } from '../../api/queries';
import type { Team } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import ui from '../../styles/ui.module.css';
import styles from './settings.module.css';
import { touchClass } from './OrganisationPage';
import orgStyles from './organisation.module.css';

const NAME_RULE = 'Enter 1 to 200 characters.';

/** A fixed sentence for a failed rename. No server text reaches the screen. */
export function renameErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400:
      return NAME_RULE;
    case 403:
      return 'You need admin access to rename a team.';
    case 404:
      return 'The server cannot find this team. Reload the page and try again.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    default:
      return 'Could not rename the team. Try again.';
  }
}

/** Rename one team (LAUN-99). The key is fixed. The sidebar and the page titles read the same teams query. */
export function RenameTeamDialog({ team, onClose }: { team: Team; onClose: () => void }) {
  const rename = useRenameTeam();
  const [name, setName] = useState(team.name);
  const [error, setError] = useState<string | null>(null);

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (rename.isPending) return;
    const n = name.trim();
    if (n.length < 1 || [...n].length > 200) {
      setError(NAME_RULE);
      document.getElementById('team-rename-name')?.focus();
      return;
    }
    if (n === team.name) return onClose();
    try {
      await rename.mutateAsync({ team: team.key, name: n });
      onClose();
    } catch (err) {
      setError(renameErrorText(err));
    }
  }

  return (
    <Dialog className={touchClass} title={`Rename team ${team.key}`} width={460} onClose={onClose}>
      <form onSubmit={(e) => void submit(e)} noValidate aria-label={`Rename team ${team.key}`}>
        <div className={ui.field}>
          <label htmlFor="team-rename-name">Team name</label>
          <input
            id="team-rename-name"
            className={ui.input}
            value={name}
            onChange={(e) => setName(e.target.value)}
            aria-invalid={!!error}
            aria-describedby={error ? 'team-rename-error team-rename-hint' : 'team-rename-hint'}
            autoComplete="off"
            data-autofocus
          />
          <span id="team-rename-hint" className={orgStyles.hint}>
            The key {team.key} is fixed. It stays in every issue id.
          </span>
          {error && (
            <span id="team-rename-error" role="alert" className={orgStyles.fieldError}>
              {error}
            </span>
          )}
        </div>
        <div className={styles.actions}>
          <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className={`${ui.btn} ${ui.primary}`}
            aria-disabled={rename.isPending || undefined}
          >
            {rename.isPending ? 'Saving' : 'Save'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

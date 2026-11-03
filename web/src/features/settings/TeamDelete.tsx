import { useState } from 'react';
import { ApiError } from '../../api/errors';
import { useDeleteTeam } from '../../api/queries';
import type { Team } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import ui from '../../styles/ui.module.css';
import styles from './settings.module.css';
import { touchClass } from './OrganisationPage';

/** The 409 text is the server's own sentence (cleaned): it names the reason and the number of issues. */
export function deleteTeamErrorText(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.status) {
      case 0:
        return 'Could not reach the server. Check your connection and try again.';
      case 403:
        return 'You need admin access to delete a team.';
      case 404:
        return 'The server cannot find this team. Reload the page and try again.';
      case 409:
        return err.message;
      case 429:
        return 'Too many requests. Wait a moment and try again.';
    }
  }
  return 'Could not delete the team. Try again.';
}

/** Delete one team (LAUN-191). The server refuses a team that has issues, is the only team or is the default team. */
export function DeleteTeamDialog({ team, onClose }: { team: Team; onClose: () => void }) {
  const del = useDeleteTeam();
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog className={touchClass} title={`Delete team ${team.key}?`} width={460} onClose={onClose}>
      <p>
        This deletes the team for good, with its workflow states, labels, cycles and notification settings. A
        team that has issues cannot be deleted, and neither can the only team or the default team of the
        organisation. Products and projects stay.
      </p>
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      <div className={styles.actions}>
        <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose} data-autofocus>
          Cancel
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.danger}`}
          disabled={del.isPending}
          onClick={() =>
            del.mutate(team.key, {
              onSuccess: onClose,
              onError: (e) => setError(deleteTeamErrorText(e)),
            })
          }
        >
          {del.isPending ? 'Deleting…' : 'Delete team'}
        </button>
      </div>
    </Dialog>
  );
}

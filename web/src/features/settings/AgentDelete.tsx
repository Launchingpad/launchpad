import { useState } from 'react';
import { ApiError } from '../../api/errors';
import { useDeleteAgent } from '../../api/queries';
import type { Member } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import ui from '../../styles/ui.module.css';
import styles from './settings.module.css';
import { touchClass } from './OrganisationPage';

export function deleteAgentErrorText(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.status) {
      case 0:
        return 'Could not reach the server. Check your connection and try again.';
      case 403:
        return 'You need admin access to delete an agent.';
      case 404:
        return 'The server cannot find this agent. It may be deleted already. Reload the page.';
      case 409:
        return err.message;
      case 429:
        return 'Too many requests. Wait a moment and try again.';
    }
  }
  return 'Could not delete the agent. Try again.';
}

/** Delete one agent (decisions-T47). Its tokens go; its issues and comments stay with no author or assignee. */
export function DeleteAgentDialog({ agent, onClose }: { agent: Member; onClose: () => void }) {
  const del = useDeleteAgent();
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog className={touchClass} title={`Delete agent ${agent.name}?`} width={460} onClose={onClose}>
      <p>
        This deletes the agent <code>{agent.username}</code> for good. All its tokens are removed, so a
        program that uses one stops working at once. The issues it created or was assigned, and its comments
        and documents, stay, with no author or assignee. This cannot be undone.
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
            del.mutate(agent.id, {
              onSuccess: onClose,
              onError: (e) => setError(deleteAgentErrorText(e)),
            })
          }
        >
          {del.isPending ? 'Deleting…' : 'Delete agent'}
        </button>
      </div>
    </Dialog>
  );
}

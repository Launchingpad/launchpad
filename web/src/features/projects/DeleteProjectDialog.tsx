import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { ApiError } from '../../api/errors';
import type { Project } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import ui from '../../styles/ui.module.css';
import { useDeleteProject } from './queries';
import styles from './projects.module.css';

function errorText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 403) return 'You need admin access to delete a project.';
    if (err.status === 404) return 'This project is already gone. Reload the page.';
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  }
  return 'Could not delete the project. Try again.';
}

/** Delete asks first and says what stays (LAUN-191): the issues and documents keep existing without a project. */
export function DeleteProjectDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const org = useOrgSlug();
  const navigate = useNavigate();
  const del = useDeleteProject();
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog title={`Delete ${project.name}?`} width={460} onClose={onClose}>
      <p>
        This deletes the project and its milestones for good. Its issues and documents are not deleted: they
        stay, with no project and no milestone.
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
            del.mutate(project.id, {
              onSuccess: () => navigate(`/${org}/projects`),
              onError: (e) => setError(errorText(e)),
            })
          }
        >
          {del.isPending ? 'Deleting…' : 'Delete project'}
        </button>
      </div>
    </Dialog>
  );
}

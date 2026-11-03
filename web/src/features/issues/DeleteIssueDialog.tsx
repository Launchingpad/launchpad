import { useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useDeleteIssue } from '../../api/queries';
import { ApiError } from '../../api/errors';
import type { Issue } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import ui from '../../styles/ui.module.css';
import detail from './detail.module.css';

export function deleteErrorText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 403) return 'You need admin access to delete an issue.';
    if (err.status === 404) return 'This issue is already gone. Reload the page.';
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  }
  return 'Could not delete the issue. Try again.';
}

/**
 * Delete asks first and says what goes and what stays (LAUN-201, decisions-T32). Cancel has focus; Escape
 * closes with no request. After a delete the person lands on the team's issue list.
 */
export function DeleteIssueDialog({ issue, onClose }: { issue: Issue; onClose: () => void }) {
  const org = useOrgSlug();
  const navigate = useNavigate();
  const del = useDeleteIssue();
  const [error, setError] = useState<string | null>(null);
  // Two activations in one tick would send two DELETEs, and the 404 of the second would hide the first's success.
  const sent = useRef(false);
  const goToList = () => navigate(`/${org}/team/${issue.team.key}`, { replace: true });
  return (
    <Dialog title={`Delete ${issue.identifier}?`} width={480} onClose={onClose}>
      <p>
        This deletes {issue.identifier} for good, with its comments, attachments, relations and activity. You
        cannot undo it. Its sub-issues are not deleted: they stay, with no parent.
      </p>
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      <div className={detail.dialogFooter}>
        <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose} data-autofocus>
          Cancel
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.dangerSolid}`}
          disabled={del.isPending}
          onClick={() => {
            if (sent.current) return;
            sent.current = true;
            del.mutate(issue.identifier, {
              onSuccess: goToList,
              onError: (e) => {
                // 404: someone else deleted it first. It is gone, which is what was asked for.
                if (e instanceof ApiError && e.status === 404) return goToList();
                sent.current = false;
                setError(deleteErrorText(e));
              },
            });
          }}
        >
          {del.isPending ? 'Deleting…' : 'Delete issue'}
        </button>
      </div>
    </Dialog>
  );
}

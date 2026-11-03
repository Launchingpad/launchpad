import { useRef, useState } from 'react';
import { ApiError } from '../../api/errors';
import type { Project } from '../../api/types';
import { relativeTime } from '../../lib/time';
import { uuid } from '../../lib/uuid';
import { ErrorState, SkeletonRows } from '../../ui/States';
import { useCreateProjectUpdate, useProjectUpdates, type ProjectUpdateHealth } from './queries';
import { UpdateScheduleForm } from './UpdateScheduleForm';
import ui from '../../styles/ui.module.css';
import styles from './projects.module.css';

export const HEALTH_LABEL: Record<ProjectUpdateHealth, string> = {
  on_track: 'On track',
  at_risk: 'At risk',
  off_track: 'Off track',
};
const HEALTHS: ProjectUpdateHealth[] = ['on_track', 'at_risk', 'off_track'];

/** The Updates section of the project page (LAUN-196): the reminder schedule, a composer and the list, newest first. */
export function ProjectUpdates({ project, canPost }: { project: Project; canPost: boolean }) {
  const id = project.id;
  const q = useProjectUpdates(id);
  const post = useCreateProjectUpdate(id);
  const [health, setHealth] = useState<ProjectUpdateHealth>('on_track');
  const [body, setBody] = useState('');
  // Set at once (state would be one render late): a second Enter or click must not start a second post.
  const inFlight = useRef(false);
  // One key per attempt. A retry of the SAME draft keeps it; a changed draft or a success makes a new one.
  const attempt = useRef<{ key: string; draft: string } | null>(null);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (inFlight.current) return;
    const draft = JSON.stringify([health, body]);
    if (!attempt.current || attempt.current.draft !== draft) attempt.current = { key: uuid(), draft };
    inFlight.current = true;
    post.mutate(
      { body: { health, body }, key: attempt.current.key },
      {
        onSuccess: () => {
          attempt.current = null;
          setBody('');
        },
        onSettled: () => {
          inFlight.current = false;
        },
      },
    );
  };

  return (
    <section aria-labelledby="updates-h" className={styles.updates}>
      <h2 id="updates-h" className={styles.h2}>
        Updates
      </h2>
      {canPost && (
        <>
          <h3 className={styles.subHead}>Reminder schedule</h3>
          <UpdateScheduleForm key={`${project.version}`} project={project} />
          <h3 className={styles.subHead}>Post an update</h3>
          <form onSubmit={submit} className={styles.updateForm} aria-label="Post an update">
            <div className={ui.field}>
              <label htmlFor="pu-health">Health</label>
              <select
                id="pu-health"
                className={ui.select}
                value={health}
                onChange={(e) => setHealth(e.target.value as ProjectUpdateHealth)}
              >
                {HEALTHS.map((h) => (
                  <option key={h} value={h}>
                    {HEALTH_LABEL[h]}
                  </option>
                ))}
              </select>
            </div>
            <div className={ui.field}>
              <label htmlFor="pu-body">Optional text</label>
              <textarea
                id="pu-body"
                className={ui.textarea}
                rows={3}
                maxLength={50000}
                placeholder="Plain text for now"
                value={body}
                onChange={(e) => setBody(e.target.value)}
              />
            </div>
            {post.isError && (
              <div role="alert">
                {post.error instanceof ApiError && post.error.message
                  ? post.error.message
                  : 'Could not post the update.'}
              </div>
            )}
            <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={post.isPending}>
              Post update
            </button>
          </form>
        </>
      )}
      {q.isPending ? (
        <SkeletonRows />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load updates" />
      ) : q.data.length === 0 ? (
        <p className={styles.muted}>No updates yet.</p>
      ) : (
        <ul className={styles.msList}>
          {q.data.map((u) => (
            <li key={u.id} className={styles.update}>
              <div className={styles.updateHead}>
                <span className={`${styles.updateHealth} ${styles[`updateHealth_${u.health}`] ?? ''}`}>
                  {HEALTH_LABEL[u.health]}
                </span>
                <span className={styles.muted}>
                  {u.author ? u.author.name : 'Someone'} · {relativeTime(u.createdAt)}
                </span>
              </div>
              {u.body && <p className={styles.updateBody}>{u.body}</p>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

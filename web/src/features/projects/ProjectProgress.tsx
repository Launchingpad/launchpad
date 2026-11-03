import type { Project } from '../../api/types';
import { pct } from './projects';
import styles from './projects.module.css';

/**
 * Progress bar and counts of one project. The counts come from the server (`issueCounts`, LAUN-170): canceled
 * issues are out of scope, sub-issues count, an empty project says "No issues".
 */
export function ProjectProgress({
  project,
  wide,
}: {
  project: Pick<Project, 'name' | 'issueCounts'>;
  wide?: boolean;
}) {
  // An older server (a mixed deploy) sends no counts: show "No issues" rather than throw.
  const { scope, done } = project.issueCounts ?? { scope: 0, done: 0 };
  const name = project.name;
  if (scope === 0) return <span className={styles.progress}>No issues</span>;
  const p = pct(done, scope);
  return (
    <span className={`${styles.progress} ${wide ? styles.progressWide : ''}`}>
      <span
        className={styles.bar}
        role="progressbar"
        aria-label={`${name} progress`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={p}
        aria-valuetext={`${p}%, ${done} of ${scope} issues done`}
      >
        <span className={styles.fill} style={{ width: `${p}%` }} />
      </span>
      <span>
        {p}% ({done}/{scope})
      </span>
    </span>
  );
}

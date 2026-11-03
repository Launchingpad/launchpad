import { Link } from 'react-router-dom';
import { useAllProjects } from '../projects/queries';
import { onCardLinkClick } from './cardLink';
import styles from './issues.module.css';

/**
 * LAUN-179: the project an issue belongs to, as a small link on list rows and board cards. It shows
 * nothing without a project, and nothing for a project the org list does not hold (an archived one).
 * A click follows the same rule as the title link (drag end, text selection, select mode on a phone).
 */
export function ProjectChip({
  org,
  projectId,
  selecting,
  onToggle,
}: {
  org: string;
  projectId: string | null;
  selecting: boolean;
  onToggle: () => void;
}) {
  const projects = useAllProjects(!!projectId);
  const project = projectId ? (projects.data ?? []).find((p) => p.id === projectId) : undefined;
  if (!project) return null;
  return (
    <Link
      to={`/${org}/projects/${project.id}`}
      className={styles.projectChip}
      title={`Project: ${project.name}`}
      aria-label={`Project: ${project.name}`}
      onClick={(e) => {
        e.stopPropagation();
        onCardLinkClick(e, selecting, onToggle);
      }}
    >
      {project.name}
    </Link>
  );
}

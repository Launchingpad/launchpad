import { memo, useRef, type KeyboardEvent, type PointerEvent as ReactPointerEvent } from 'react';
import { Link } from 'react-router-dom';
import type { Issue } from '../../api/types';
import { relativeTime, fullDate } from '../../lib/time';
import { Avatar } from '../../ui/Avatar';
import { PriorityIcon, StatusIcon } from '../../ui/Icons';
import { LabelPill } from '../../ui/LabelPill';
import { useAllProjects } from '../projects/queries';
import { onCardClick, onCardLinkClick } from './cardLink';
import { ROW_PILLS, hiddenLabels, labelsTitle, useClippedPills } from './labelOverflow';
import { ProjectChip } from './ProjectChip';
import styles from './issues.module.css';

function RowImpl({
  issue,
  org,
  focused,
  selected,
  selecting = false,
  onFocus,
  onToggle,
  onHandlePress,
  onHandleKey,
}: {
  issue: Issue;
  org: string;
  focused: boolean;
  selected: boolean;
  selecting?: boolean;
  onFocus: (id: string) => void;
  onToggle: (id: string) => void;
  /** LAUN-111: given only with sort Manual. The drag handle is then shown and starts the drag. */
  onHandlePress?: (e: ReactPointerEvent<HTMLElement>, issue: Issue) => void;
  onHandleKey?: (e: KeyboardEvent<HTMLElement>, issue: Issue) => void;
}) {
  const reorder = !!onHandlePress;
  // LAUN-233: the pills sit in one clipped line; "+N" counts every label not shown and is outside the clip.
  const labelsRef = useRef<HTMLSpanElement>(null);
  const clipped = useClippedPills(labelsRef);
  const hidden = hiddenLabels(issue.labels, ROW_PILLS, clipped);
  const projects = useAllProjects(!!issue.projectId);
  const projectName = issue.projectId
    ? projects.data?.find((p) => p.id === issue.projectId)?.name
    : undefined;
  return (
    <li
      className={`${styles.row} ${reorder ? styles.rowDrag : ''} ${focused ? styles.focused : ''} ${selected ? styles.selected : ''}`}
      data-issue-id={issue.id}
      data-team-id={issue.team.id}
      aria-current={focused ? 'true' : undefined}
      onMouseDown={() => onFocus(issue.id)}
      onClick={onCardClick}
    >
      {reorder && (
        <button
          type="button"
          className={styles.handle}
          data-drag-handle=""
          aria-label={`Move ${issue.identifier}`}
          title="Drag to change its place, or press Up or Down"
          onPointerDown={(e) => onHandlePress(e, issue)}
          onKeyDown={(e) => onHandleKey?.(e, issue)}
          onClick={(e) => e.stopPropagation()}
          onDragStart={(e) => e.preventDefault()}
        >
          <svg width="10" height="14" viewBox="0 0 10 14" aria-hidden="true" focusable="false">
            <g fill="currentColor">
              <circle cx="2.5" cy="2.5" r="1.3" />
              <circle cx="7.5" cy="2.5" r="1.3" />
              <circle cx="2.5" cy="7" r="1.3" />
              <circle cx="7.5" cy="7" r="1.3" />
              <circle cx="2.5" cy="11.5" r="1.3" />
              <circle cx="7.5" cy="11.5" r="1.3" />
            </g>
          </svg>
        </button>
      )}
      <input
        type="checkbox"
        className={styles.check}
        checked={selected}
        onChange={() => onToggle(issue.id)}
        aria-label={`Select ${issue.identifier}`}
      />
      <span className={styles.prio} title={issue.priorityLabel}>
        <PriorityIcon priority={issue.priority} />
        <span className="sr-only">{issue.priorityLabel}</span>
      </span>
      <span className={styles.ident}>{issue.identifier}</span>
      <span className={styles.status} title={issue.state.name}>
        <StatusIcon type={issue.state.type} color={issue.state.color} name={issue.state.name} />
        <span className="sr-only">{issue.state.name}</span>
      </span>
      <Link
        to={`/${org}/issue/${issue.identifier}`}
        className={styles.title}
        state={{ issue }}
        onClick={(e) => onCardLinkClick(e, selecting, () => onToggle(issue.id))}
      >
        <span className={styles.titleText}>{issue.title}</span>
      </Link>
      <span className={styles.labelsCell} title={labelsTitle(projectName, issue.labels)}>
        <span ref={labelsRef} className={styles.labels}>
          <ProjectChip
            org={org}
            projectId={issue.projectId}
            selecting={selecting}
            onToggle={() => onToggle(issue.id)}
          />
          {issue.labels.slice(0, ROW_PILLS).map((l) => (
            <LabelPill key={l.id} label={l} />
          ))}
        </span>
        {hidden.length > 0 && (
          <span className={styles.more} title={hidden.map((l) => l.name).join(', ')}>
            +{hidden.length}
          </span>
        )}
      </span>
      <span className={styles.assignee} title={issue.assignee?.name ?? 'Unassigned'}>
        {issue.assignee ? (
          <Avatar name={issue.assignee.name} username={issue.assignee.username} />
        ) : (
          <span className={styles.noAssignee} aria-hidden="true" />
        )}
        <span className="sr-only">{issue.assignee?.name ?? 'Unassigned'}</span>
      </span>
      <time className={styles.updated} dateTime={issue.updatedAt} title={fullDate(issue.updatedAt)}>
        {relativeTime(issue.updatedAt)}
      </time>
    </li>
  );
}
export const IssueRow = memo(RowImpl);

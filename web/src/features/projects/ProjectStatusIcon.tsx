import type { StateType } from '../../api/types';
import { StatusIcon } from '../../ui/Icons';
import type { ProjectState } from './projects';

/** The workflow glyph for a project status KIND: the app's StatusIcon, so the shapes match the issue states. */
const KIND_TYPE: Record<ProjectState, StateType> = {
  backlog: 'backlog',
  planned: 'unstarted',
  started: 'started',
  paused: 'triage',
  completed: 'completed',
  canceled: 'canceled',
};

/** A status as a badge: the kind's icon in the status colour, then the name. The text keeps the theme colour. */
export function ProjectStatusBadge({
  status,
  className,
}: {
  status: { kind: ProjectState; name: string; color: string };
  className?: string;
}) {
  return (
    <span className={className} title={status.name}>
      <ProjectStatusIcon kind={status.kind} color={status.color} />
      <span>{status.name}</span>
    </span>
  );
}

/** The kind's shape in the status's own colour (LAUN-195). */
export function ProjectStatusIcon({ kind, color }: { kind: ProjectState; color?: string }) {
  return <StatusIcon type={KIND_TYPE[kind]} color={color || 'var(--muted)'} />;
}

import type { Group } from '../../lib/group';
import { Avatar } from '../../ui/Avatar';
import { PriorityIcon, StatusIcon } from '../../ui/Icons';

/** LAUN-171: the small mark before a board column's name; it depends on what the board groups by. */
export function ColumnGlyph({ group: g }: { group: Group }) {
  switch (g.kind) {
    case 'assignee':
      return typeof g.value === 'string' ? <Avatar name={g.name} username={g.value} /> : null;
    case 'priority':
      return <PriorityIcon priority={Number(g.value)} />;
    case 'label':
      return g.tint ? (
        <span
          aria-hidden="true"
          style={{ width: 10, height: 10, borderRadius: '50%', background: g.tint, display: 'inline-block' }}
        />
      ) : null;
    case 'project':
      return null;
    default:
      return <StatusIcon type={g.type} color={g.color} name={g.name} />;
  }
}

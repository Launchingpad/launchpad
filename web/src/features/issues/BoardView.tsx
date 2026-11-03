import {
  memo,
  useCallback,
  useLayoutEffect,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from 'react';
import { Link } from 'react-router-dom';
import type { Issue } from '../../api/types';
import type { Placement } from './dropPosition';
import { groupKeyOf, type Group } from '../../lib/group';
import { ColumnGlyph } from './ColumnGlyph';
import type { GroupKey } from './filters';
import { priorityLabel as priorityName } from '../../lib/priority';
import { isCoarsePointer, modKeyName } from '../../lib/platform';
import { shortDate } from '../../lib/time';
import { Avatar } from '../../ui/Avatar';
import { IconPlus, PriorityIcon } from '../../ui/Icons';
import { LabelPill } from '../../ui/LabelPill';
import { useBoardDrag, type DropRequest } from './boardDrag';
import { onCardClick, onCardLinkClick } from './cardLink';
import { useScrollRestore } from '../../lib/backNav';
import { ProjectChip } from './ProjectChip';
import styles from './issues.module.css';
import { useDragScroll } from './useDragScroll';

/**
 * One card. Memoised: a drag changes no React state (boardDrag.ts moves cards with style writes),
 * and a focus or selection change re-renders only the card whose props changed.
 */
const BoardCard = memo(function BoardCard({
  issue: i,
  org,
  focused,
  selected,
  selecting,
  onFocus,
  onToggle,
  onPress,
  dragOn,
}: {
  issue: Issue;
  dragOn: boolean;
  org: string;
  focused: boolean;
  selected: boolean;
  selecting: boolean;
  onFocus: (id: string) => void;
  onToggle: (id: string) => void;
  onPress: (e: ReactPointerEvent<HTMLElement>, issue: Issue) => void;
}) {
  return (
    <li
      data-issue-id={i.id}
      data-team-id={i.team.id}
      data-drag-off={dragOn ? undefined : ''}
      className={`${styles.card} ${focused ? styles.focused : ''} ${selected ? styles.selected : ''}`}
      // The browser's own drag (a link or an image) would start a drag ghost and cancel our pointer events.
      onDragStart={(e) => e.preventDefault()}
      onPointerDown={(e) => onPress(e, i)}
      onMouseDown={() => onFocus(i.id)}
      onClick={onCardClick}
    >
      <div className={styles.cardTop}>
        <input
          type="checkbox"
          className={styles.cardCheck}
          checked={selected}
          onChange={() => onToggle(i.id)}
          aria-label={`Select ${i.identifier}`}
        />
        <span className={styles.ident}>{i.identifier}</span>
        <span className={styles.spacer} />
        {i.assignee && <Avatar name={i.assignee.name} username={i.assignee.username} />}
      </div>
      <Link
        to={`/${org}/issue/${i.identifier}`}
        state={{ issue: i }}
        className={styles.cardTitle}
        onClick={(e) => onCardLinkClick(e, selecting, () => onToggle(i.id))}
      >
        <span className={styles.titleText}>{i.title}</span>
      </Link>
      <div className={styles.cardMeta}>
        <PriorityIcon priority={i.priority} />
        <span className="sr-only">{i.priorityLabel}</span>
        <ProjectChip
          org={org}
          projectId={i.projectId}
          selecting={selecting}
          onToggle={() => onToggle(i.id)}
        />
        {i.labels.slice(0, 2).map((l) => (
          <LabelPill key={l.id} label={l} />
        ))}
      </div>
      <time className={styles.cardDate} dateTime={i.createdAt}>
        Created {shortDate(i.createdAt)}
      </time>
    </li>
  );
});

/** The column's select-all box: checked when every loaded card is selected, mixed when some are. */
function ColumnCheck({
  name,
  ids,
  selected,
  onToggleColumn,
}: {
  name: string;
  ids: string[];
  selected: Set<string>;
  onToggleColumn: (ids: string[], on: boolean) => void;
}) {
  const n = ids.filter((id) => selected.has(id)).length;
  const all = n === ids.length;
  return (
    <input
      type="checkbox"
      className={styles.colCheck}
      checked={all}
      ref={(el) => {
        if (el) el.indeterminate = n > 0 && !all;
      }}
      onChange={() => onToggleColumn(ids, !all)}
      aria-label={all ? `Clear all in ${name}` : `Select all ${ids.length} loaded in ${name}`}
    />
  );
}

/** The scrolling card list of one column. Back puts its inner scroll back (LAUN-90). */
function ColumnCards({ name, pending, children }: { name: string; pending: boolean; children: ReactNode }) {
  const ref = useScrollRestore<HTMLUListElement>(`col:${name}`, undefined, true, () => pending);
  return (
    <ul ref={ref} className={styles.cards} data-cards="">
      {children}
    </ul>
  );
}

export function BoardView({
  columns,
  org,
  focusedId,
  selected,
  selecting = false,
  onFocus,
  onToggle,
  onDrop,
  onDropGroup,
  onToggleColumn,
  loadingMore,
  reorder = true,
  onCreate,
  kind = 'state',
  dragOn = true,
  sortName,
  priorityDrop = false,
  onDropValue,
}: {
  columns: Group[];
  org: string;
  focusedId: string | null;
  selected: Set<string>;
  selecting?: boolean;
  onFocus: (id: string) => void;
  onToggle: (id: string) => void;
  /** A card was dropped (LAUN-29): its column and the neighbour it landed next to. One update. */
  onDrop: (
    issue: Issue,
    column: string,
    placement: DropRequest['placement'],
    /** LAUN-171: the priority to set with the move (a drop with Ctrl or Command, see `priorityDrop`). */
    priority?: number,
  ) => void;
  /** LAUN-61: selected cards were dropped as a block (the held card is one of them). */
  onDropGroup?: (
    issues: Issue[],
    column: string,
    base: Record<string, Placement>,
    stateOnly?: boolean,
    priority?: number,
  ) => void;
  /** LAUN-61: the column's header checkbox: select (on) or clear (off) every card of the column. */
  onToggleColumn?: (ids: string[], on: boolean) => void;
  loadingMore?: boolean;
  /**
   * LAUN-167: false when the sort is not Manual. A card can still be dragged to ANOTHER column (the state
   * changes, no place is sent); a drop in its own column changes nothing (the order shown is not the stored one).
   */
  reorder?: boolean;
  /** LAUN-173: the "+" at the foot of a lane: open the New issue dialog with this lane's state. */
  onCreate?: (state: string) => void;
  /**
   * LAUN-171: what the columns are. A state column's drop sets the state; an assignee or priority column's drop
   * sets that value (`onDropValue`); label and project columns do not take a drop (`dragOn` false).
   */
  kind?: GroupKey;
  /** False for label and project columns: the cards open as usual and do not drag. */
  dragOn?: boolean;
  /** The sort's name, when it is not Manual: the hint over a drag says "Board ordered by <name>". */
  sortName?: string;
  /** Holding Ctrl or Command at the drop also changes the priority (the board is not Manual, see decisions-T23). */
  priorityDrop?: boolean;
  /** LAUN-171: cards were dropped on a column that is not a state; `priority` is set too with Ctrl or Command. */
  onDropValue?: (issues: Issue[], column: Group, priority?: number) => void;
}) {
  const boardRef = useRef<HTMLDivElement>(null);
  const latest = useRef({ columns, selected });
  latest.current = { columns, selected };
  const [live, setLive] = useState('');
  const [dragging, setDragging] = useState(false);
  // LAUN-89: Back puts the sideways scroll back. The board loads its own pages (LAUN-90): wait while some are due.
  useScrollRestore('board', boardRef, true, () => !!loadingMore);
  useDragScroll(boardRef, styles.dragScrolling!); // LAUN-55: drag on empty space scrolls sideways
  const drag = useBoardDrag({
    getBoard: () => boardRef.current,
    onDrop: (r) => {
      const col = latest.current.columns.find((c) => c.key === r.column);
      const colName = col?.name ?? r.column;
      // LAUN-171: Ctrl or Command at the drop. The card takes the priority of the card it landed next to.
      const near =
        priorityDrop && r.alsoPriority && r.neighbourId
          ? latest.current.columns.flatMap((c) => c.issues).find((i) => i.id === r.neighbourId)
          : undefined;
      const priority = near?.priority;
      const said = priority !== undefined ? ` and priority set to ${priorityName(priority)}` : '';
      if (kind !== 'state') {
        // An assignee or priority column: the drop sets that value; there is no order to save.
        const held = r.group?.issues ?? [r.issue];
        const moving = held.filter(
          (i) => groupKeyOf(kind, i) !== r.column || (priority !== undefined && priority !== i.priority),
        );
        if (!moving.length || !col || !onDropValue) {
          setLive(`Nothing moved: reordering inside ${colName} is not saved`);
          return;
        }
        setLive(
          `${moving.length === 1 ? moving[0]!.identifier : `${moving.length} issues`} moved to ${colName}${said}`,
        );
        onDropValue(moving, col, kind === 'priority' ? undefined : priority);
        return;
      }
      if (!reorder) {
        // LAUN-167: not Manual. Only the state changes (and the priority with Ctrl or Command, LAUN-171); the card
        // lands where the sort puts it. The same-column branches below are defensive: boardDrag cancels such a
        // drop first (`reorder`), unless the key is held.
        if (r.group && onDropGroup) {
          const moving = r.group.issues.filter(
            (i) => i.state.name !== r.column || (priority !== undefined && priority !== i.priority),
          );
          if (!moving.length) {
            setLive(`Nothing moved: reordering inside ${r.column} needs the Manual sort`);
            return;
          }
          setLive(`${moving.length} ${moving.length === 1 ? 'issue' : 'issues'} moved to ${r.column}${said}`);
          onDropGroup(moving, r.column, {}, true, priority);
          return;
        }
        if (r.issue.state.name === r.column && (priority === undefined || priority === r.issue.priority)) {
          setLive(`${r.issue.identifier} was not moved: reordering inside ${r.column} needs the Manual sort`);
          return;
        }
        setLive(
          r.issue.state.name === r.column
            ? `${r.issue.identifier} priority set to ${priorityName(priority!)}`
            : `${r.issue.identifier} moved to ${r.column}${said}`,
        );
        onDrop(r.issue, r.column, {}, priority);
        return;
      }
      if (r.group && onDropGroup) {
        const n = r.group.issues.length;
        setLive(`${n} issues moved to ${r.column}`);
        onDropGroup(r.group.issues, r.column, r.group.base);
        return;
      }
      setLive(
        r.issue.state.name === r.column
          ? `${r.issue.identifier} moved to position ${r.index + 1} of ${r.count} in ${r.column}`
          : `${r.issue.identifier} moved to ${r.column}, position ${r.index + 1} of ${r.count}`,
      );
      onDrop(r.issue, r.column, r.placement);
    },
    announce: setLive,
    reorder: () => reorder,
    isIn: (i, key) => groupKeyOf(kind, i) === key,
    alsoPriority: () => priorityDrop,
    sameColumnSay: (n) =>
      kind === 'state'
        ? `Reordering inside ${n} needs the Manual sort`
        : `Reordering inside ${n} is not saved`,
    onActive: setDragging,
  });
  // Pressing a selected card holds the whole selection (LAUN-61), in board order.
  const onPress = useCallback(
    (e: ReactPointerEvent<HTMLElement>, issue: Issue) => {
      if (!dragOn) return; // label and project columns: the card opens, it does not drag
      const { columns: cols, selected: sel } = latest.current;
      const group =
        sel.size > 1 && sel.has(issue.id) ? cols.flatMap((c) => c.issues).filter((i) => sel.has(i.id)) : [];
      drag.press(e, issue, group);
    },
    [drag, dragOn],
  );
  // New data while a drag is on (an event, a refetch): measure again. The drop's own update: done.
  useLayoutEffect(() => drag.columnsChanged(), [drag, columns]);

  return (
    <div ref={boardRef} className={styles.board} data-hscroll="" role="group" aria-label="Board">
      {columns.map((c) => (
        <section
          key={c.key}
          className={styles.column}
          aria-label={`${c.name}, ${c.issues.length} issues`}
          data-column={c.key}
          data-name={c.name}
        >
          <h2 className={styles.colHead}>
            {onToggleColumn && c.issues.length > 0 && (
              <ColumnCheck
                name={c.name}
                ids={c.issues.map((i) => i.id)}
                selected={selected}
                onToggleColumn={onToggleColumn}
              />
            )}
            <ColumnGlyph group={c} /> {c.name} <span className={styles.count}>{c.issues.length}</span>
          </h2>
          <ColumnCards name={c.key} pending={!!loadingMore}>
            {c.issues.map((i) => (
              <BoardCard
                key={i.id}
                issue={i}
                org={org}
                focused={focusedId === i.id}
                selected={selected.has(i.id)}
                selecting={selecting}
                onFocus={onFocus}
                onToggle={onToggle}
                onPress={onPress}
                dragOn={dragOn}
              />
            ))}
            {/* The gray placeholder: shown by boardDrag.ts in the gap while a card is dragged. */}
            {/* Only a state column saves a place (LAUN-171): the other kinds draw no gap. */}
            {kind === 'state' && (
              <li className={styles.dropGap} data-gap="" role="presentation" aria-hidden="true" />
            )}
          </ColumnCards>
          {onCreate && (
            <div className={styles.colFoot}>
              <button
                type="button"
                className={styles.colAdd}
                aria-label={`Create issue in ${c.name}`}
                title={`Create issue in ${c.name}`}
                onClick={() => onCreate(c.name)}
              >
                <IconPlus />
              </button>
            </div>
          )}
        </section>
      ))}
      {dragging && sortName && (
        <div className={styles.dragHint} data-testid="drag-hint" aria-hidden="true">
          <strong>Board ordered by {sortName}</strong>
          {priorityDrop && !isCoarsePointer() && <span>Hold {modKeyName()} to also change priority</span>}
        </div>
      )}
      <div className="sr-only" aria-live="polite" data-testid="board-live">
        {live}
      </div>
      {loadingMore && (
        <div className={styles.sentinel} role="status">
          Loading more…
        </div>
      )}
    </div>
  );
}

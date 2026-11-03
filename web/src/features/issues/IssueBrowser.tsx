import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useAllStates, useIssuesInfinite, useLabels, useMembers } from '../../api/queries';
import { useAllProjects } from '../projects/queries';
import type { Issue } from '../../api/types';
import { groupByState, boardGroups, type Group } from '../../lib/group';
import { useHotkeys } from '../../lib/hotkeys';
import { EmptyState, ErrorState, SkeletonRows, Spinner } from '../../ui/States';
import { StatusIcon } from '../../ui/Icons';
import { FilterBar } from './FilterBar';
import { IssueRow } from './IssueRow';
import { BoardView } from './BoardView';
import { ColumnsControl } from './ColumnsControl';
import { applyColumns, entriesOf } from './boardColumns';
import { defaultColumns, type ColumnPrefs } from './viewPrefs';
import { useListDrag } from './listDrag';
import { stepPlacement } from './listDropTarget';
import { activeFilterCount, type IssueApiQuery, type IssueFilters } from './filters';
import { useIssueActions } from './actions';
import { useIssueUndo } from './useIssueUndo';
import { useUi } from '../shell/ui-context';
import { QuickPicker, type QuickKind } from './QuickPicker';
import { nextPageStep, useScrollRestore } from '../../lib/backNav';
import ui from '../../styles/ui.module.css';
import styles from './issues.module.css';

type Quick = QuickKind | null;

const SORT_NAMES = {
  updated: 'Updated',
  created: 'Created',
  priority: 'Priority',
  manual: 'Manual',
} as const;

/**
 * The filter bar, the list or board, selection, hotkeys and bulk pickers.
 * The issues pages and the cycle page (LAUN-18) both render it; each owns
 * where `filters` live (the URL) and what `extra` narrows the query to.
 */
export function IssueBrowser({
  filters,
  onChange,
  onResetView,
  columns = defaultColumns,
  onColumnsChange,
  extra,
  title,
  lockAssignee,
  lockTeam,
  lockProduct,
  lockProject,
  scopeTeams,
  blocked,
  emptyTitle,
  emptyNone,
}: {
  filters: IssueFilters;
  onChange: (f: IssueFilters) => void;
  /** LAUN-109: set while this place has a remembered or non-default view; the "Reset view" button calls it. */
  onResetView?: () => void;
  /** LAUN-163: the board's column choices of this place, and the way to remember a change. */
  columns?: ColumnPrefs;
  onColumnsChange?: (c: ColumnPrefs) => void;
  /** Extra API parameters, such as `cycle`. */
  extra?: Partial<IssueApiQuery>;
  /** The page heading (the cycle page has its own head). */
  title?: ReactNode;
  lockAssignee?: boolean;
  lockTeam?: string;
  /** The product is fixed by `extra` (the product page): no Product chip. */
  lockProduct?: boolean;
  lockProject?: boolean;
  /** Team keys whose states make the columns and the Status chip (the product page, LAUN-65). */
  scopeTeams?: string[];
  /** Shown in place of the rows (for example: the org has no team). */
  blocked?: ReactNode;
  /** The empty state when nothing is filtered: its title and its action. */
  emptyTitle: string;
  emptyNone?: ReactNode;
}) {
  const { openCreate } = useUi();
  const org = useOrgSlug();
  const nav = useNavigate();
  const setFilters = onChange;
  const q = useIssuesInfinite(filters, extra);
  // LAUN-90: Back may need more pages than the first fetch to reach the saved offset: load them (at most 10).
  // Read the flags in render (TanStack tracks the fields a component reads while it renders; a field read only
  // inside the callback would not re-render the component, and the callback would see stale values).
  const { hasNextPage: more, isFetchingNextPage: moreBusy, isFetchNextPageError: moreFailed } = q;
  const pagesLoaded = q.data?.pages.length ?? 0;
  const { key: entry, pathname } = useLocation();
  const asked = useRef('');
  const scrollRef = useScrollRestore<HTMLDivElement>('issues', undefined, filters.view !== 'board', () => {
    const step = nextPageStep({
      hasNextPage: more,
      failed: moreFailed,
      busy: moreBusy,
      pages: pagesLoaded,
      asked: asked.current,
      entry,
    });
    if (step.fetch) {
      asked.current = `${entry}|${pagesLoaded}`;
      void q.fetchNextPage();
    }
    return step.wait;
  });
  const { states: allStates, teams: allTeams } = useAllStates();
  const scopeKey = scopeTeams?.join(',');
  const states = useMemo(() => {
    if (scopeTeams === undefined) return allStates;
    const keys = new Set((scopeKey ?? '').toLowerCase().split(',').filter(Boolean));
    const ids = new Set(allTeams.filter((t) => keys.has(t.key.toLowerCase())).map((t) => t.id));
    return allStates.filter((s) => ids.has(s.teamId));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [allStates, allTeams, scopeKey]);
  // LAUN-236, LAUN-237: Cmd/Ctrl+Z on the board and the list, one hook. Another team, product, cycle or project,
  // or the other view: a new stack.
  const undo = useIssueUndo({
    view: filters.view === 'board' ? 'board' : 'list',
    // The path too: All issues and My issues are the same page instance (LAUN-237 review).
    scope: `${org}|${pathname}|${lockAssignee ? 'mine' : ''}|${lockTeam ?? filters.team ?? ''}|${scopeKey ?? ''}|${JSON.stringify(extra ?? {})}`,
  });
  const actions = useIssueActions(undefined, undo.recorder);
  const issues = useMemo(() => q.data?.pages.flatMap((p) => p.items) ?? [], [q.data]);
  const board = filters.view === 'board';
  // LAUN-171: what the board's columns are. The list always groups by state.
  const kind = board ? filters.group : 'state';
  const members = useMembers();
  const labelList = useLabels(lockTeam ?? filters.team, board && kind === 'label');
  const projectList = useAllProjects(board && kind === 'project');
  const groups = useMemo(
    () =>
      board
        ? boardGroups(
            kind,
            issues,
            {
              states,
              members: members.data ?? [],
              labels: labelList.data ?? [],
              projects: projectList.data ?? [],
            },
            filters.sort,
          ) /* Manual: the server's order; Priority: sorted here */
        : groupByState(issues, states, filters.sort),
    [board, kind, issues, states, members.data, labelList.data, projectList.data, filters.sort],
  );
  // LAUN-163: hidden columns leave the board, the keyboard walk, the selection and the drag. Their cards still count
  // in `hiddenIssues` and in every column's own count.
  const {
    shown: boardShownColumns,
    hiddenColumns,
    hiddenIssues,
  } = useMemo(
    () =>
      board ? applyColumns(groups, columns, kind) : { shown: groups, hiddenColumns: 0, hiddenIssues: 0 },
    [board, groups, columns, kind],
  );
  const flat = useMemo(() => boardShownColumns.flatMap((g) => g.issues), [boardShownColumns]);

  const [focusedId, setFocusedId] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [quick, setQuick] = useState<Quick>(null);
  // LAUN-16: on a narrow screen the checkboxes show only in select mode (or once something is selected).
  const [selectMode, setSelectMode] = useState(false);
  const selecting = selectMode || selected.size > 0;

  // Drop selection of rows that left the list (filter change, another tab).
  useEffect(() => {
    setSelected((s) => {
      const ids = new Set(flat.map((i) => i.id));
      const next = new Set([...s].filter((id) => ids.has(id)));
      return next.size === s.size ? s : next;
    });
  }, [flat]);

  // Infinite scroll. The board needs every card, so it keeps loading.
  const sentinel = useRef<HTMLDivElement>(null);
  const { hasNextPage, isFetchingNextPage, fetchNextPage } = q;
  useEffect(() => {
    const el = sentinel.current;
    if (!el || !hasNextPage || board) return;
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting) && !isFetchingNextPage) void fetchNextPage();
      },
      { rootMargin: '400px' },
    );
    io.observe(el);
    return () => io.disconnect();
  }, [hasNextPage, isFetchingNextPage, fetchNextPage, board, flat.length]);
  const pages = q.data?.pages.length ?? 0;
  useEffect(() => {
    if (board && hasNextPage && !isFetchingNextPage && pages < 20) void fetchNextPage();
  }, [board, hasNextPage, isFetchingNextPage, fetchNextPage, pages]);

  const move = (delta: number) => {
    if (!flat.length) return;
    const i = flat.findIndex((x) => x.id === focusedId);
    const next =
      flat[Math.min(flat.length - 1, Math.max(0, i < 0 ? (delta > 0 ? 0 : flat.length - 1) : i + delta))]!;
    setFocusedId(next.id);
    document.querySelector(`[data-issue-id="${next.id}"]`)?.scrollIntoView?.({ block: 'nearest' });
  };
  const toggle = useCallback(
    (id: string) =>
      setSelected((s) => {
        const n = new Set(s);
        if (n.has(id)) n.delete(id);
        else n.add(id);
        return n;
      }),
    [],
  );
  const toggleColumn = useCallback(
    (ids: string[], on: boolean) =>
      setSelected((s) => {
        const n = new Set(s);
        for (const id of ids) {
          if (on) n.add(id);
          else n.delete(id);
        }
        return n;
      }),
    [],
  );
  const targets: Issue[] = useMemo(() => {
    if (selected.size) return flat.filter((i) => selected.has(i.id));
    const f = flat.find((i) => i.id === focusedId);
    return f ? [f] : [];
  }, [flat, selected, focusedId]);
  const openQuick = (k: Exclude<Quick, null>) => {
    if (targets.length) setQuick(k);
  };

  useHotkeys({
    j: () => move(1),
    k: () => move(-1),
    Enter: () => {
      const f = flat.find((i) => i.id === focusedId);
      if (f) nav(`/${org}/issue/${f.identifier}`, { state: { issue: f } });
    },
    x: () => focusedId && toggle(focusedId),
    s: () => openQuick('status'),
    a: () => openQuick('assignee'),
    l: () => openQuick('label'),
    p: () => openQuick('priority'),
    Escape: () => {
      setSelected(new Set());
      setSelectMode(false);
    },
  });

  // LAUN-111: drag a list row by its handle (sort Manual only), or move it with Up / Down on the handle.
  const listRef = useRef<HTMLDivElement>(null);
  const [live, setLive] = useState('');
  const listDragOn = !board && filters.sort === 'manual';
  const latestGroups = useRef(groups);
  latestGroups.current = groups;
  const refocus = useRef<string | null>(null);
  const refocusTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const listDrag = useListDrag({
    getList: () => listRef.current,
    getScroller: () => scrollRef.current,
    onDrop: (r) => {
      // LAUN-131: say "moved" only after the move is known to have happened. `place` answers false when it
      // refused (the team lacks the state) or the update failed and was rolled back.
      void actions
        .place(r.issue, r.column, r.placement)
        .then((ok) =>
          setLive(
            !ok
              ? `${r.issue.identifier} could not be moved`
              : r.issue.state.name === r.column
                ? `${r.issue.identifier} moved to position ${r.index + 1} of ${r.count} in ${r.column}`
                : `${r.issue.identifier} moved to ${r.column}, position ${r.index + 1} of ${r.count}`,
          ),
        );
    },
    announce: setLive,
  });
  const onHandlePress = useCallback(
    (e: ReactPointerEvent<HTMLElement>, issue: Issue) => listDrag.press(e, issue),
    [listDrag],
  );
  const placeIssue = actions.place;
  const placeRef = useRef(placeIssue);
  placeRef.current = placeIssue;
  // LAUN-131: `latestGroups` changes only after a render, so a second key press before it would compute from
  // the old order. While moves are in flight the order of each group is kept HERE, updated at once.
  const keyOrder = useRef(new Map<string, { id: string; teamId: string }[]>());
  const keyPending = useRef(0);
  const onHandleKey = useCallback((e: ReactKeyboardEvent<HTMLElement>, issue: Issue) => {
    if ((e.key !== 'ArrowUp' && e.key !== 'ArrowDown') || e.metaKey || e.ctrlKey || e.altKey || e.shiftKey)
      return;
    e.preventDefault();
    e.stopPropagation();
    const g = latestGroups.current.find((x) => x.name === issue.state.name);
    if (!g) return;
    const rows = keyOrder.current.get(g.name) ?? g.issues.map((i) => ({ id: i.id, teamId: i.team.id }));
    const step = stepPlacement(rows, issue.id, e.key === 'ArrowUp' ? -1 : 1);
    if (!step) {
      setLive(`${issue.identifier} cannot move further among the loaded issues`);
      return;
    }
    const rest = rows.filter((r) => r.id !== issue.id);
    const me = rows.find((r) => r.id === issue.id)!;
    keyOrder.current.set(g.name, [...rest.slice(0, step.index), me, ...rest.slice(step.index)]);
    keyPending.current += 1;
    refocus.current = issue.id;
    clearTimeout(refocusTimer.current);
    refocusTimer.current = setTimeout(() => (refocus.current = null), 800); // the update has rendered by then
    void placeRef.current(issue, g.name, step.placement).then((ok) => {
      keyPending.current -= 1;
      if (!ok || keyPending.current === 0) keyOrder.current.clear();
      setLive(
        ok
          ? `${issue.identifier} moved to position ${step.index + 1} of ${rows.length} in ${g.name}`
          : `${issue.identifier} could not be moved`,
      );
    });
  }, []);
  // The row moves in the DOM (or is drawn again): keep the keyboard on its handle, unless the person has
  // since moved on (focus is on something else).
  useLayoutEffect(() => {
    const id = refocus.current;
    if (!id) return;
    const a = document.activeElement;
    if (a && a !== document.body) return; // still on the handle, or the person went elsewhere
    const h = listRef.current?.querySelector<HTMLElement>(`[data-issue-id="${id}"] [data-drag-handle]`);
    if (h && document.activeElement !== h) h.focus();
  });
  // The data changed under a drag, or the drop's own update has rendered.
  useLayoutEffect(() => listDrag.rowsChanged(), [listDrag, groups]);

  const filtered = activeFilterCount(filters) > 0;

  // With a title the count sits in the head row. Without one (the cycle page) it floats, so the bar and the rows do not move.
  const selCount = selecting ? (
    <span className={styles.selCount} role="status">
      {selected.size} selected
      <button
        type="button"
        className={`${ui.btn} ${ui.ghost}`}
        onClick={() => {
          setSelected(new Set());
          setSelectMode(false);
        }}
      >
        Clear
      </button>
    </span>
  ) : null;

  // "Show all columns" clears the choices of the group kind on screen only (LAUN-171: each kind keeps its own).
  const showAllColumns = () =>
    onColumnsChange?.({
      ...columns,
      hideEmpty: false,
      hidden: columns.hidden.filter((n) => !entriesOf(kind, columns.hidden).includes(n)),
    });
  // LAUN-171: a drop on an assignee or priority column sets that value (one update per card).
  const dropValue = (held: Issue[], col: Group, priority?: number) => {
    setSelected(new Set()); // the selection is spent by a drop
    let fields: Parameters<typeof actions.setFields>[1];
    if (kind === 'assignee') {
      const m = col.value === null ? null : members.data?.find((x) => x.username === col.value);
      if (m === undefined) return; // not an active member: nothing to set
      fields = { assignee: m, priority };
    } else fields = { priority: Number(col.value) };
    void Promise.all(held.map((i) => actions.setFields(i, fields)));
  };
  const dragOn = kind !== 'label' && kind !== 'project';
  const notManual = filters.sort !== 'manual';
  const boardShown = board && !blocked && !q.isPending && !q.isError;

  return (
    <div className={`${styles.page} ${selecting ? styles.selecting : ''}`}>
      {title && (
        <div className={styles.pageHead}>
          {title}
          {selCount}
        </div>
      )}
      <FilterBar
        filters={filters}
        onChange={setFilters}
        onResetView={onResetView}
        columnsControl={
          board && onColumnsChange ? (
            <ColumnsControl all={groups} prefs={columns} onChange={onColumnsChange} kind={kind} />
          ) : undefined
        }
        lockAssignee={lockAssignee}
        lockTeam={lockTeam}
        lockProduct={lockProduct}
        lockProject={lockProject}
        scopeTeams={scopeTeams}
        selectMode={selecting}
        onSelectMode={(on) => {
          setSelectMode(on);
          if (!on) setSelected(new Set());
        }}
      />
      <div
        ref={scrollRef}
        className={`${styles.scroll} ${boardShown ? styles.scrollBoard : ''}`}
        data-testid="issues-scroll"
      >
        {blocked ? (
          blocked
        ) : q.isPending ? (
          <SkeletonRows />
        ) : q.isError ? (
          <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load issues" />
        ) : flat.length === 0 && !board ? (
          <EmptyState title={filtered ? 'No issues match these filters' : emptyTitle}>
            {filtered ? (
              <button
                type="button"
                className={ui.btn}
                onClick={() =>
                  setFilters({
                    ...filters,
                    state: [],
                    label: [],
                    priority: undefined,
                    q: '',
                    assignee: undefined,
                    project: undefined,
                  })
                }
              >
                Clear filters
              </button>
            ) : (
              emptyNone
            )}
          </EmptyState>
        ) : board ? (
          <>
            <div className={styles.boardHint} data-testid="board-hint">
              {hiddenColumns > 0 && boardShownColumns.length > 0 && onColumnsChange && (
                <span className={styles.boardHidden} data-testid="board-hidden">
                  {hiddenColumns} {hiddenColumns === 1 ? 'column' : 'columns'} hidden, holding {hiddenIssues}{' '}
                  {hiddenIssues === 1 ? 'issue' : 'issues'}.
                  <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={showAllColumns}>
                    Show all columns
                  </button>
                </span>
              )}
              {kind === 'label' || kind === 'project' ? (
                <span data-testid="drag-off-hint">
                  {kind === 'label'
                    ? 'Dragging is off when the board is grouped by label: a card can have several labels. A card with several shows under its first. Open a card to change its labels.'
                    : 'Dragging is off when the board is grouped by project: changing a project is not a quick move. Open a card to change its project.'}
                </span>
              ) : kind !== 'state' ? (
                <>
                  {notManual && `Sort is ${SORT_NAMES[filters.sort]}. `}
                  {kind === 'assignee'
                    ? 'Drag a card to another column to change its assignee; "Unassigned" clears it.'
                    : 'Drag a card to another column to change its priority.'}
                  {' The order inside a column is not saved.'}
                </>
              ) : filters.sort === 'manual' ? (
                'Drag a card to change its place. The order is saved.'
              ) : (
                <>
                  Sort is {SORT_NAMES[filters.sort]}. Drag a card to another column to change its status;
                  switch to Manual to reorder inside a column.
                  <button
                    type="button"
                    className={`${ui.btn} ${ui.ghost}`}
                    onClick={() => setFilters({ ...filters, sort: 'manual' })}
                  >
                    Switch to Manual
                  </button>
                </>
              )}
            </div>
            {boardShownColumns.length === 0 ? (
              <EmptyState title={filtered ? 'No columns to show for these filters' : 'No columns to show'}>
                {onColumnsChange && (
                  <button type="button" className={ui.btn} onClick={showAllColumns}>
                    Show all columns
                  </button>
                )}
              </EmptyState>
            ) : (
              <BoardView
                columns={boardShownColumns}
                org={org}
                focusedId={focusedId}
                selected={selected}
                selecting={selecting}
                onFocus={setFocusedId}
                onToggle={toggle}
                onDrop={(issue, column, placement, priority) =>
                  void actions.place(issue, column, placement, priority)
                }
                onToggleColumn={toggleColumn}
                onDropGroup={(issues, column, base, stateOnly, priority) => {
                  setSelected(new Set()); // the selection is spent by a drop
                  void actions.placeMany(issues, column, base, stateOnly, priority);
                }}
                kind={kind}
                dragOn={dragOn}
                sortName={notManual && dragOn ? SORT_NAMES[filters.sort] : undefined}
                priorityDrop={notManual && (kind === 'state' || kind === 'assignee')}
                onDropValue={dropValue}
                loadingMore={hasNextPage}
                reorder={filters.sort === 'manual' && kind === 'state'}
                onCreate={
                  kind !== 'state'
                    ? undefined
                    : (state) =>
                        openCreate({
                          state,
                          team: lockTeam ?? filters.team,
                          ...(scopeTeams?.length ? { firstTeams: scopeTeams } : {}),
                        })
                }
              />
            )}
          </>
        ) : (
          <>
            <div className={styles.boardHint} data-testid="list-hint">
              {listDragOn ? (
                'Drag the handle at the left of a row to change its place. The order is saved.'
              ) : (
                <>
                  Sort is {SORT_NAMES[filters.sort]}: switch to Manual to reorder.
                  <button
                    type="button"
                    className={`${ui.btn} ${ui.ghost}`}
                    onClick={() => setFilters({ ...filters, sort: 'manual' })}
                  >
                    Switch to Manual
                  </button>
                </>
              )}
            </div>
            <div ref={listRef} className={styles.listWrap} data-list="">
              {groups.map((g) => (
                <section key={g.key} aria-label={g.name} className={styles.group} data-group={g.name}>
                  <h2 className={styles.groupHead}>
                    <StatusIcon type={g.type} color={g.color} name={g.name} /> {g.name}{' '}
                    <span className={styles.count}>{g.issues.length}</span>
                  </h2>
                  <ul className={styles.list}>
                    {g.issues.map((i) => (
                      <IssueRow
                        key={i.id}
                        issue={i}
                        org={org}
                        focused={focusedId === i.id}
                        selected={selected.has(i.id)}
                        selecting={selecting}
                        onFocus={setFocusedId}
                        onToggle={toggle}
                        onHandlePress={listDragOn ? onHandlePress : undefined}
                        onHandleKey={listDragOn ? onHandleKey : undefined}
                      />
                    ))}
                  </ul>
                </section>
              ))}
            </div>
            <div className="sr-only" aria-live="polite" data-testid="list-live">
              {live}
            </div>
            <div ref={sentinel} className={styles.sentinel}>
              {isFetchingNextPage ? (
                <Spinner label="Loading more" />
              ) : hasNextPage ? (
                <button type="button" className={ui.btn} onClick={() => void fetchNextPage()}>
                  Load more
                </button>
              ) : (
                <span className={ui.hint}>{flat.length} issues</span>
              )}
            </div>
          </>
        )}
      </div>

      {!title && selCount && <div className={styles.selFloat}>{selCount}</div>}
      {quick && (
        <QuickPicker kind={quick} targets={targets} actions={actions} onClose={() => setQuick(null)} />
      )}
    </div>
  );
}

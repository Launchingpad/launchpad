import { useCallback, useEffect, useMemo } from 'react';
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import type { Cycle } from '../../api/types';
import { usePageTitle } from '../../lib/documentTitle';
import { useCycles, useTeams } from '../../api/queries';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { IconCycle } from '../../ui/Icons';
import { Breadcrumbs } from '../../ui/Breadcrumbs';
import { Menu, type MenuItem } from '../../ui/Menu';
import { IssueBrowser } from '../issues/IssueBrowser';
import { parseFilters, writeFilters, type IssueFilters } from '../issues/filters';
import { placeKey, useRememberedView } from '../issues/viewPrefs';
import { cycleName, cycleKind, dateRange, neighbours, percent, shortRange, switcherCycles } from './cycles';
import { BurnUp } from './BurnUp';
import { CycleOptions } from './CycleOptions';
import { useCycleProgress, useCycleSnapshots } from './queries';
import { useNow } from './useNow';
import { useScrollRestore } from '../../lib/backNav';
import styles from './cycles.module.css';

function CycleIssues({ team, number }: { team: string; number: number }) {
  const [sp, setSp] = useSearchParams();
  const parsed = useMemo(() => parseFilters(sp), [sp]);
  const extra = useMemo(() => ({ cycle: String(number) }), [number]);
  const write = useCallback(
    (f: IssueFilters) => setSp(writeFilters({ ...f, team: undefined }), { replace: true }),
    [setSp],
  );
  // LAUN-109: each cycle page has its own remembered layout and sort.
  const remembered = useRememberedView(placeKey('cycle', team, number), sp, parsed, write);
  // The team comes from the path, not from the query string.
  const filters = useMemo(() => ({ ...remembered.filters, team }), [remembered.filters, team]);
  const onChange = remembered.setFilters;
  return (
    <IssueBrowser
      filters={filters}
      onChange={onChange}
      onResetView={remembered.customised ? remembered.resetView : undefined}
      columns={remembered.columns}
      onColumnsChange={remembered.setColumns}
      extra={extra}
      lockTeam={team}
      emptyTitle="No issues in this cycle"
    />
  );
}

export function CyclePage() {
  const org = useOrgSlug();
  const { team = '', number = '' } = useParams();
  const teams = useTeams();
  const teamRow = teams.data?.find((t) => t.key.toLowerCase() === team.toLowerCase());
  const n = /^[1-9][0-9]{0,8}$/.test(number) ? Number(number) : undefined;
  const cycles = useCycles(teamRow?.key, !!teamRow?.cyclesEnabled);
  const cycle = cycles.data?.find((c) => c.number === n);

  const now = useNow();
  const kind = cycle ? cycleKind(cycle, now) : undefined;
  const progress = useCycleProgress(cycle?.id ?? '', {
    enabled: !!cycle && kind !== 'next',
    // Only a completed cycle has final rows; one that just ended is not completed until the job runs.
    final: !!cycle?.completedAt,
  });
  const snaps = useCycleSnapshots(cycle?.id ?? '', {
    enabled: !!cycle && kind !== 'next',
    final: !!cycle?.completedAt,
  });
  const teamKey = teamRow?.key ?? team;
  const listUrl = `/${org}/team/${teamKey}/cycles`;
  const nav = useNavigate();
  const near = useMemo(
    () => (cycle && cycles.data ? neighbours(cycles.data, cycle) : { prev: null, next: null }),
    [cycle, cycles.data],
  );
  const shown = useMemo(
    () =>
      cycle && cycles.data ? switcherCycles(cycles.data, cycle, now) : { prev: null, next: null, here: null },
    [cycle, cycles.data, now],
  );
  const open = useCallback(
    (c: { number: number }) => nav(`/${org}/team/${teamKey}/cycle/${c.number}`),
    [nav, org, teamKey],
  );

  // Alt+K: the next cycle, Alt+J: the previous one. `code`, because Option+K types another character on a Mac.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (
        e.defaultPrevented ||
        e.isComposing ||
        e.repeat ||
        !e.altKey ||
        e.ctrlKey ||
        e.metaKey ||
        e.shiftKey
      )
        return;
      const el = e.target as HTMLElement | null;
      if (el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName))) return;
      if (
        el?.closest('[role="dialog"],[aria-modal="true"]') ||
        document.querySelector('[role="dialog"],[aria-modal="true"]')
      )
        return;
      const to = e.code === 'KeyK' ? near.next : e.code === 'KeyJ' ? near.prev : null;
      if (!to) return;
      e.preventDefault();
      open(to);
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [near, open]);

  const zone = teamRow?.cycleTimezone;
  const entry = (c: Cycle, hint: string): MenuItem => ({
    label: cycleName(c),
    detail: shortRange(c, zone),
    hint,
    icon: <IconCycle />,
    onSelect: () => open(c),
  });
  const items: MenuItem[] = [];
  // LAUN-116: a past cycle is not listed here (see "All cycles"), except the one being viewed.
  if (shown.here)
    items.push(
      { label: 'This cycle', heading: true, onSelect: () => {} },
      {
        ...entry(shown.here, ''),
        hint: undefined,
        detail: `Past, ${shortRange(shown.here, zone)}`,
        current: true,
        onSelect: () => {},
      },
    );
  if (shown.next)
    items.push({ label: 'Next cycle', heading: true, onSelect: () => {} }, entry(shown.next, 'Alt+K'));
  if (shown.prev)
    items.push({ label: 'Previous cycle', heading: true, onSelect: () => {} }, entry(shown.prev, 'Alt+J'));
  items.push({ label: 'All cycles', onSelect: () => nav(listUrl) });
  const title = cycle ? cycleName(cycle) : `Cycle ${number.slice(0, 12)}`;
  usePageTitle(title);

  const head = (
    <div className={styles.head}>
      <Breadcrumbs
        items={[
          { key: 'team', label: teamRow?.name ?? teamKey, to: `/${org}/team/${teamKey}` },
          { key: 'cycles', label: 'Cycles', to: listUrl },
          {
            key: 'cycle',
            label: title,
            heading: true,
            after: cycle && (
              <Menu
                closeKey={cycle.id}
                label="Switch cycle"
                trigger={null}
                triggerClassName={styles.switch}
                wrapClassName={styles.switchWrap}
                panelClassName={styles.switchPanel}
                items={items}
              />
            ),
          },
        ]}
      />
      {cycle && (
        <>
          <CycleOptions
            cycle={cycle}
            next={near.next}
            zone={zone}
            teamKey={teamKey}
            rollover={teamRow?.cycleRollover ?? true}
            now={now}
          />
          <span className={styles.sub}>
            {dateRange(cycle, zone, now)}
            {kind === 'current' ? ' (current)' : kind === 'next' ? ' (next)' : ''}
          </span>
          {progress.data && (
            <span className={styles.sub}>
              {percent(progress.data.done, progress.data.scope)}% done ({progress.data.done}/
              {progress.data.scope})
            </span>
          )}
        </>
      )}
    </div>
  );

  let body;
  let browser = false;
  if (teams.isPending || (teamRow?.cyclesEnabled && cycles.isPending)) body = <SkeletonRows />;
  else if (teams.isError)
    body = (
      <ErrorState error={teams.error} onRetry={() => void teams.refetch()} title="Could not load teams" />
    );
  else if (cycles.isError)
    body = (
      <ErrorState
        error={cycles.error}
        onRetry={() => void cycles.refetch()}
        title="Could not load the cycle"
      />
    );
  else if (!teamRow || !teamRow.cyclesEnabled || n === undefined || !cycle)
    body = (
      <EmptyState title="Cycle not found">
        <p>
          <Link to={listUrl}>Back to the cycles</Link>
        </p>
      </EmptyState>
    );
  else {
    browser = true;
    body = <CycleIssues key={cycle.number} team={teamRow.key} number={cycle.number} />;
  }

  const scrollRef = useScrollRestore<HTMLDivElement>('cycle', undefined, !browser);
  return (
    <div className={styles.page}>
      {head}
      {browser && cycle && !(snaps.isError && !snaps.data) && !snaps.isLoading && (
        <BurnUp cycle={cycle} rows={snaps.data} zone={zone} now={now} upcoming={kind === 'next'} />
      )}
      {/* The issue browser has its own scroll area and filter bar. */}
      {browser ? (
        body
      ) : (
        <div ref={scrollRef} className={styles.scroll}>
          {body}
        </div>
      )}
    </div>
  );
}

import { useMemo, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { usePageTitle } from '../../lib/documentTitle';
import { useEntryMemory, useScrollRestore } from '../../lib/backNav';
import { useCycles, useTeams } from '../../api/queries';
import type { Cycle } from '../../api/types';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { cycleName, dateRange, percent, sectionCycles, type CycleKind } from './cycles';
import { useNow } from './useNow';
import { useCycleProgress } from './queries';
import ui from '../../styles/ui.module.css';
import styles from './cycles.module.css';

const PAST_SHOWN = 10;

function Progress({ cycle, kind }: { cycle: Cycle; kind: CycleKind }) {
  const q = useCycleProgress(cycle.id, { enabled: kind !== 'next', final: !!cycle.completedAt });
  if (kind === 'next') return <span className={styles.progress}>Not started</span>;
  if (q.isPending)
    return (
      <span className={styles.progress} role="status" aria-label="Loading progress">
        …
      </span>
    );
  if (q.isError) return <span className={styles.progress}>Progress unavailable</span>;
  if (!q.data) return <span className={styles.progress}>No data yet</span>;
  const pct = percent(q.data.done, q.data.scope);
  return (
    <span className={styles.progress}>
      <span
        className={styles.bar}
        role="progressbar"
        aria-label={`${cycleName(cycle)} progress`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct}
      >
        <span className={styles.fill} style={{ width: `${pct}%` }} />
      </span>
      <span>
        {pct}% ({q.data.done}/{q.data.scope})
      </span>
    </span>
  );
}

function CycleRow({
  cycle,
  base,
  tag,
  kind,
  zone,
  now,
}: {
  cycle: Cycle;
  base: string;
  tag?: string;
  kind: CycleKind;
  zone: string;
  now: number;
}) {
  return (
    <li className={styles.row}>
      <Link to={`${base}/${cycle.number}`} className={styles.title}>
        <span className={styles.num} aria-hidden="true">
          #{cycle.number}
        </span>
        {cycleName(cycle)}
        {tag && (
          <>
            {' '}
            <span className={styles.tag}>{tag}</span>
          </>
        )}
      </Link>
      <span className={styles.dates}>{dateRange(cycle, zone, now)}</span>
      <Progress cycle={cycle} kind={kind} />
    </li>
  );
}

export function CycleListPage() {
  const org = useOrgSlug();
  const { team = '' } = useParams();
  const teams = useTeams();
  const teamRow = teams.data?.find((t) => t.key.toLowerCase() === team.toLowerCase());
  const q = useCycles(teamRow?.key, !!teamRow?.cyclesEnabled);
  // Back keeps "Show all past cycles" too, or a saved offset in the long list would be out of reach (LAUN-90).
  const [savedAll, saveAll] = useEntryMemory('allPast');
  const [allPast, setAllPast] = useState(savedAll === '1');
  const scrollRef = useScrollRestore<HTMLDivElement>('cycles');
  const now = useNow();
  const sections = useMemo(() => sectionCycles(q.data ?? [], now), [q.data, now]);
  const base = `/${org}/team/${teamRow?.key ?? team}/cycle`;
  usePageTitle('Cycles');

  let body;
  if (teams.isPending) body = <SkeletonRows />;
  else if (teams.isError)
    body = (
      <ErrorState error={teams.error} onRetry={() => void teams.refetch()} title="Could not load teams" />
    );
  else if (!teamRow) body = <EmptyState title="Team not found" />;
  else if (!teamRow.cyclesEnabled)
    body = (
      <EmptyState title="Cycles are off for this team">
        <p>An organisation admin can turn cycles on in the team settings.</p>
      </EmptyState>
    );
  else if (q.isPending) body = <SkeletonRows />;
  else if (q.isError)
    body = <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load cycles" />;
  else if (q.data.length === 0)
    body = (
      <EmptyState title="No cycles yet">
        <p>The first cycle appears within an hour of turning cycles on.</p>
      </EmptyState>
    );
  else {
    const past = allPast ? sections.past : sections.past.slice(0, PAST_SHOWN);
    const block = (title: string, list: Cycle[], kind: CycleKind, tag?: string) =>
      list.length > 0 && (
        <section aria-label={title}>
          <h2 className={styles.sectionHead}>{title}</h2>
          <ul className={styles.list}>
            {list.map((c) => (
              <CycleRow
                key={c.id}
                cycle={c}
                base={base}
                tag={tag}
                kind={kind}
                zone={teamRow.cycleTimezone}
                now={now}
              />
            ))}
          </ul>
        </section>
      );
    body = (
      <>
        {block('Current', sections.current, 'current', 'Current')}
        {block('Next', sections.next, 'next')}
        {block('Past', past, 'past')}
        {sections.past.length > PAST_SHOWN && !allPast && (
          <div className={styles.more}>
            <button
              type="button"
              className={ui.btn}
              onClick={() => {
                saveAll('1');
                setAllPast(true);
              }}
            >
              Show all {sections.past.length} past cycles
            </button>
          </div>
        )}
      </>
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.head}>
        <h1 className={styles.h1}>{teamRow ? `${teamRow.name} cycles` : 'Cycles'}</h1>
        {teamRow && (
          <Link to={`/${org}/team/${teamRow.key}`} className={styles.sub}>
            All issues of the team
          </Link>
        )}
      </div>
      <div ref={scrollRef} className={styles.scroll}>
        {body}
      </div>
    </div>
  );
}

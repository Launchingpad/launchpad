import { Fragment, useCallback, useMemo, useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useProjectStatuses } from '../../api/projectStatuses';
import { useProducts, useTeams } from '../../api/queries';
import type { Project } from '../../api/types';
import { usePageTitle } from '../../lib/documentTitle';
import { useScrollRestore } from '../../lib/backNav';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { Logo } from '../../ui/Logo';
import { LabelPill } from '../../ui/LabelPill';
import ui from '../../styles/ui.module.css';
import { ProjectStatusBadge } from './ProjectStatusIcon';
import { ProjectFormDialog } from './ProjectFormDialog';
import { ProjectProgress } from './ProjectProgress';
import { formatDay, groupByProduct, isOverdue, todayString, type ProjectGroup } from './projects';
import { ProjectFilterBar } from './ProjectFilterBar';
import {
  activeProjectFilters,
  COLUMNS,
  emptyProjectFilters,
  filterProjects,
  HEALTH_LABEL,
  healthOf,
  readView,
  sortBy,
  writeView,
  type ColumnId,
  type SortDir,
  type SortKey,
} from './projectsView';
import { useAllProjects } from './queries';
import styles from './projects.module.css';

/** localStorage key of the "Group by product" choice (LAUN-166). Per browser; every read and write can throw. */
export const GROUP_KEY = 'launchpad.projects.group-by-product.v1';

function readGroup(): boolean {
  try {
    return localStorage.getItem(GROUP_KEY) !== 'off';
  } catch {
    return true;
  }
}

const SORTABLE: Partial<Record<ColumnId, SortKey>> = {
  health: 'health',
  lead: 'lead',
  target: 'target',
  status: 'status',
};

function HealthPill({ p, today }: { p: Project; today: string }) {
  const h = healthOf(p, today);
  return <span className={`${styles.health} ${styles[`health_${h}`] ?? ''}`}>{HEALTH_LABEL[h]}</span>;
}

function ProjectRow({
  p,
  org,
  today,
  cols,
}: {
  p: Project;
  org: string;
  today: string;
  cols: Set<ColumnId>;
}) {
  return (
    <tr className={styles.tr}>
      <th scope="row" className={styles.nameCell}>
        <Link to={`/${org}/projects/${p.id}`} className={styles.title}>
          <Logo name={p.name} src={p.avatarUrl} emoji={p.emoji} size={20} decorative />
          <span className={styles.name}>{p.name}</span>
        </Link>
      </th>
      {cols.has('labels') && (
        <td className={`${styles.cell} ${styles.labelsCell}`}>
          <span className={styles.srOnly}>Labels: </span>
          {p.labels.length === 0 ? (
            <span className={styles.muted}>No labels</span>
          ) : (
            <span className={styles.chips}>
              {p.labels.map((l) => (
                <LabelPill key={l.id} label={l} />
              ))}
            </span>
          )}
        </td>
      )}
      {cols.has('health') && (
        <td className={styles.cell}>
          <span className={styles.srOnly}>Health: </span>
          <HealthPill p={p} today={today} />
        </td>
      )}
      {cols.has('priority') && (
        <td className={styles.cell}>
          <span className={styles.srOnly}>Priority: </span>
          <span className={styles.muted}>No priority</span>
        </td>
      )}
      {cols.has('lead') && (
        <td className={`${styles.cell} ${styles.lead}`}>
          <span className={styles.srOnly}>Lead: </span>
          {p.lead ? p.lead.name : <span className={styles.muted}>No lead</span>}
        </td>
      )}
      {cols.has('target') && (
        <td className={`${styles.cell} ${isOverdue(p, today) ? styles.overdue : styles.dates}`}>
          {p.targetDate ? (
            <>
              <span className={styles.srOnly}>Target date </span>
              {formatDay(p.targetDate)}
              {isOverdue(p, today) && <span className={styles.srOnly}> (overdue)</span>}
            </>
          ) : (
            <span className={styles.muted}>No target date</span>
          )}
        </td>
      )}
      {cols.has('status') && (
        <td className={styles.cell}>
          <span className={styles.srOnly}>Status: </span>
          <ProjectStatusBadge status={p.status} className={styles.state} />
        </td>
      )}
      {cols.has('progress') && (
        <td className={styles.cell}>
          <span className={styles.srOnly}>Progress: </span>
          <ProjectProgress project={p} />
        </td>
      )}
    </tr>
  );
}

function ProjectTable({
  groups,
  rows,
  org,
  today,
  cols,
  sort,
  dir,
  onSort,
}: {
  groups: ProjectGroup[] | null;
  rows: Project[];
  org: string;
  today: string;
  cols: Set<ColumnId>;
  sort: SortKey;
  dir: SortDir;
  onSort: (key: SortKey, dir: SortDir) => void;
}) {
  const sortHead = (key: SortKey, label: string) => {
    const on = sort === key;
    return (
      <th scope="col" aria-sort={on ? (dir === 'asc' ? 'ascending' : 'descending') : undefined}>
        <button
          type="button"
          className={styles.sortBtn}
          onClick={() => onSort(key, on && dir === 'asc' ? 'desc' : 'asc')}
        >
          {label}
          <span aria-hidden="true" className={styles.sortArrow}>
            {on ? (dir === 'asc' ? '\u2191' : '\u2193') : ''}
          </span>
        </button>
      </th>
    );
  };
  const plainHead = (label: string) => <th scope="col">{label}</th>;
  const span = 1 + cols.size;
  return (
    <table className={styles.table} aria-label="Projects">
      <thead className={styles.thead}>
        <tr>
          {sortHead('name', 'Name')}
          {COLUMNS.filter((c) => cols.has(c.id)).map((c) => {
            const key = SORTABLE[c.id];
            return key ? (
              <Fragment key={c.id}>{sortHead(key, c.label)}</Fragment>
            ) : (
              <Fragment key={c.id}>{plainHead(c.label)}</Fragment>
            );
          })}
        </tr>
      </thead>
      {groups ? (
        groups.map((g) => (
          <tbody key={g.id || 'none'} aria-labelledby={`pg-${g.id || 'none'}`}>
            <tr className={styles.groupRow}>
              <th colSpan={span} scope="rowgroup" id={`pg-${g.id || 'none'}`} className={styles.groupHead}>
                <h2 className={styles.groupH2}>
                  {g.name} <span className={styles.groupCount}>{g.projects.length}</span>
                </h2>
              </th>
            </tr>
            {g.projects.map((p) => (
              <ProjectRow key={p.id} p={p} org={org} today={today} cols={cols} />
            ))}
          </tbody>
        ))
      ) : (
        <tbody>
          {rows.map((p) => (
            <ProjectRow key={p.id} p={p} org={org} today={today} cols={cols} />
          ))}
        </tbody>
      )}
    </table>
  );
}

/** `/:org/projects` (LAUN-119): every live project with its status, lead, target date and progress. */
export function ProjectListPage() {
  const org = useOrgSlug();
  const navigate = useNavigate();
  const q = useAllProjects();
  const products = useProducts();
  const statuses = useProjectStatuses();
  const scrollRef = useScrollRestore<HTMLDivElement>('projects');
  const [view, setView] = useState(readView);
  // The label filter lives in the address (?label=id,id), so a filtered list can be linked (LAUN-194).
  const [params, setParams] = useSearchParams();
  const labelParam = params.get('label') ?? '';
  const urlLabels = useMemo(() => labelParam.split(',').filter(Boolean), [labelParam]);
  const withLabels = useMemo(() => ({ ...view.filters, labels: urlLabels }), [view.filters, urlLabels]);
  const [grouping, setGrouping] = useState(readGroup);
  const [creating, setCreating] = useState(false);
  const [clearN, setClearN] = useState(0);
  const today = todayString();
  usePageTitle('Projects');

  const changeView = useCallback((next: typeof view) => {
    setView(next);
    writeView(next);
  }, []);
  const onFilters = useCallback(
    (next: typeof filters) => {
      const { labels, ...rest } = next;
      changeView({ ...view, filters: { ...rest, labels: [] } });
      if (labels.join(',') !== labelParam) {
        const sp = new URLSearchParams(params);
        if (labels.length) sp.set('label', labels.join(','));
        else sp.delete('label');
        setParams(sp, { replace: true });
      }
    },
    [changeView, view, labelParam, params, setParams],
  );
  const onSort = (sort: SortKey, dir: SortDir) => changeView({ ...view, sort, dir });
  const cols = useMemo(() => new Set<ColumnId>(view.columns), [view.columns]);
  // A remembered status filter may name a status that is gone (or an old kind word): drop it once the list is known.
  // A remembered team filter may name a team the person no longer sees (decisions-T20 #71) or one that is gone:
  // drop it the same way, so the chip never names it. Until the visible list is known (loading, or the read
  // failed) no remembered team is used or shown. The stored view is per browser, so it may come from another org.
  const teams = useTeams();
  const filters = useMemo(() => {
    let f = withLabels;
    if (statuses.data) {
      const known = new Set(statuses.data.map((s) => s.id));
      f = { ...f, states: f.states.filter((s) => known.has(s)) };
    }
    const visible = new Set(teams.isSuccess ? teams.data.map((t) => t.key.toLowerCase()) : []);
    f = { ...f, teams: f.teams.filter((k) => visible.has(k.toLowerCase())) };
    return f;
  }, [withLabels, statuses.data, teams.isSuccess, teams.data]);

  const all = q.data ?? [];
  const productTeamKeys = useMemo(
    () => new Map((products.data ?? []).map((p) => [p.slug, p.teamKeys])),
    [products.data],
  );
  const rows = useMemo(
    () => sortBy(filterProjects(q.data ?? [], filters, productTeamKeys), view.sort, view.dir, today),
    [q.data, filters, view.sort, view.dir, productTeamKeys, today],
  );

  // A failed or empty products read leaves the list flat, as before products existed.
  const hasProducts = (products.data?.length ?? 0) > 0;
  const groups = useMemo(
    () => (hasProducts && grouping ? groupByProduct(rows, products.data ?? []) : null),
    [hasProducts, grouping, rows, products.data],
  );

  function setGroup(on: boolean) {
    setGrouping(on);
    try {
      localStorage.setItem(GROUP_KEY, on ? 'on' : 'off');
    } catch {
      /* not remembered */
    }
  }

  const newButton = (
    <button type="button" className={`${ui.btn} ${ui.primary}`} onClick={() => setCreating(true)}>
      New project
    </button>
  );

  let body;
  if (q.isPending) body = <SkeletonRows />;
  else if (q.isError)
    body = <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load projects" />;
  else if (all.length === 0)
    body = (
      <EmptyState title="No projects yet">
        <p>A project groups issues toward one goal.</p>
        {newButton}
      </EmptyState>
    );
  else if (rows.length === 0)
    body = (
      <EmptyState title="No projects match">
        <p>No project fits these filters.</p>
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost}`}
          onClick={() => {
            setClearN((n) => n + 1);
            onFilters({ ...emptyProjectFilters });
          }}
        >
          Clear filters
        </button>
      </EmptyState>
    );
  else
    body = (
      <>
        {groups && (
          <p className={styles.groupHint}>
            A project with teams in several products appears under the product with most of them.
          </p>
        )}
        <ProjectTable
          groups={groups}
          rows={rows}
          org={org}
          today={today}
          cols={cols}
          sort={view.sort}
          dir={view.dir}
          onSort={onSort}
        />
      </>
    );

  return (
    <div className={styles.page}>
      <div className={styles.head}>
        <h1 className={styles.h1}>Projects</h1>
        {q.isSuccess && all.length > 0 && (
          <span className={styles.sub}>
            {activeProjectFilters(filters) > 0 ? `${rows.length} of ${all.length}` : all.length}
          </span>
        )}
        <span className={styles.headSpacer} />
        {hasProducts && all.length > 0 && (
          <label className={styles.groupToggle}>
            <input type="checkbox" checked={grouping} onChange={(e) => setGroup(e.target.checked)} />
            Group by product
          </label>
        )}
        {q.isSuccess && all.length > 0 && newButton}
      </div>
      {q.isSuccess && all.length > 0 && (
        <ProjectFilterBar
          clearSignal={clearN}
          filters={filters}
          onFilters={onFilters}
          columns={view.columns}
          onColumns={(columns) => changeView({ ...view, columns })}
          sort={view.sort}
          dir={view.dir}
          onSort={onSort}
        />
      )}
      <div ref={scrollRef} className={styles.scroll}>
        {body}
      </div>
      {creating && (
        <ProjectFormDialog
          onClose={() => setCreating(false)}
          onSaved={(p) => void navigate(`/${org}/projects/${p.id}`)}
        />
      )}
    </div>
  );
}

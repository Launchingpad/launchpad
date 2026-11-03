import { useCallback, useMemo, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useProject } from '../../api/queries';
import { ApiError } from '../../api/errors';
import type { Project } from '../../api/types';
import { usePageTitle } from '../../lib/documentTitle';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { IssueBrowser } from '../issues/IssueBrowser';
import { parseFilters, writeFilters, type IssueFilters } from '../issues/filters';
import { placeKey, useRememberedView } from '../issues/viewPrefs';
import { useUi } from '../shell/ui-context';
import { Logo } from '../../ui/Logo';
import { LabelPill } from '../../ui/LabelPill';
import { DeleteProjectDialog } from './DeleteProjectDialog';
import { ProjectFormDialog } from './ProjectFormDialog';
import { useIsOrgAdmin } from '../settings/access';
import { ProjectEmojiField } from './ProjectEmojiField';
import { ProjectLogoField } from './ProjectLogoField';
import { ProjectProgress } from './ProjectProgress';
import { ProjectUpdates } from './ProjectUpdates';
import { formatDay, isOverdue, todayString } from './projects';
import { useMilestones } from './queries';
import ui from '../../styles/ui.module.css';
import issueStyles from '../issues/issues.module.css';
import { ProjectStatusBadge } from './ProjectStatusIcon';
import styles from './projects.module.css';

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** `/:org/projects/:project` (LAUN-119): the project's facts, its milestones and its issues (the shared issue list). */
export function ProjectPage() {
  const { project = '' } = useParams();
  const valid = UUID.test(project);
  const q = useProject(project, valid);
  const missing =
    !valid || (q.error instanceof ApiError && (q.error.status === 404 || q.error.status === 400));
  usePageTitle(missing ? 'Not found' : q.data?.name);

  if (missing) return <EmptyState title="Project not found" />;
  if (q.isPending) return <SkeletonRows />;
  if (q.isError)
    return <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load project" />;
  return <Body p={q.data} />;
}

function Milestones({ id }: { id: string }) {
  const q = useMilestones(id);
  if (q.isPending) return <SkeletonRows />;
  if (q.isError)
    return <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load milestones" />;
  if (q.data.length === 0) return null;
  const sorted = [...q.data].sort((a, b) => a.sortOrder - b.sortOrder);
  return (
    <section aria-labelledby="milestones-h" className={styles.milestones}>
      <h2 id="milestones-h" className={styles.h2}>
        Milestones
      </h2>
      <ul className={styles.msList}>
        {sorted.map((m) => (
          <li key={m.id} className={styles.ms}>
            <span className={styles.msName}>{m.name}</span>
            <span className={styles.dates}>
              {m.targetDate ? (
                <>
                  <span className={styles.srOnly}>Target date </span>
                  {formatDay(m.targetDate)}
                </>
              ) : (
                <span className={styles.muted}>No target date</span>
              )}
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}

function Body({ p }: { p: Project }) {
  const org = useOrgSlug();
  const [sp, setSp] = useSearchParams();
  const { openCreate } = useUi();
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const isAdmin = useIsOrgAdmin();
  const keys = useMemo(() => p.teams.map((t) => t.key), [p.teams]);
  const archived = p.archivedAt !== null;
  const open = p.state !== 'completed' && p.state !== 'canceled' && !archived;

  // The project is fixed by the page: a stray ?team= or ?product= in the address is dropped.
  const parsed = useMemo(() => parseFilters(sp), [sp]);
  const write = useCallback(
    (f: IssueFilters) =>
      setSp(writeFilters({ ...f, product: undefined, team: undefined }), { replace: true }),
    [setSp],
  );
  const remembered = useRememberedView(placeKey('project', p.id), sp, parsed, write);
  const filters: IssueFilters = useMemo(
    () => ({ ...remembered.filters, product: undefined, project: undefined, team: undefined }),
    [remembered.filters],
  );
  const extra = useMemo(() => ({ project: p.id }), [p.id]);
  const create = () => openCreate({ firstTeams: keys });
  const overdue = isOverdue(p, todayString());

  return (
    <div className={issueStyles.page}>
      <div className={styles.pHead}>
        <nav aria-label="Breadcrumb" className={styles.crumb}>
          <Link to={`/${org}/projects`}>Projects</Link>
        </nav>
        <div className={styles.titleRow}>
          <Logo name={p.name} src={p.avatarUrl} emoji={p.emoji} size={28} decorative />
          <h1 className={`${issueStyles.h1} ${styles.pTitle}`}>{p.name}</h1>
          <ProjectStatusBadge status={p.status} className={styles.state} />
          <span className={styles.spacer} />
          <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={() => setEditing(true)}>
            Edit project
          </button>
          {isAdmin && (
            <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={() => setDeleting(true)}>
              Delete project
            </button>
          )}
          {open && keys.length > 0 && (
            <button type="button" className={`${ui.btn} ${ui.primary}`} onClick={create}>
              Create issue
            </button>
          )}
        </div>
        {p.description && <p className={styles.description}>{p.description}</p>}
        <dl className={styles.facts}>
          <div>
            <dt>Lead</dt>
            <dd>{p.lead ? p.lead.name : <span className={styles.muted}>No lead</span>}</dd>
          </div>
          <div>
            <dt>Start</dt>
            <dd>{p.startDate ? formatDay(p.startDate) : <span className={styles.muted}>Not set</span>}</dd>
          </div>
          <div>
            <dt>Target</dt>
            <dd className={overdue ? styles.overdue : undefined}>
              {p.targetDate ? formatDay(p.targetDate) : <span className={styles.muted}>Not set</span>}
              {overdue && <span className={styles.srOnly}> (overdue)</span>}
            </dd>
          </div>
          <div>
            <dt>Progress</dt>
            <dd>
              <ProjectProgress project={p} wide />
            </dd>
          </div>
        </dl>
        {p.labels.length > 0 && (
          <ul className={styles.labelRow} aria-label="Project labels">
            {p.labels.map((l) => (
              <li key={l.id}>
                <LabelPill label={l} />
              </li>
            ))}
          </ul>
        )}
        {p.teams.length > 0 && (
          <ul className={styles.teams} aria-label="Teams in this project">
            {p.teams.map((t) => (
              <li key={t.id}>
                <Link to={`/${org}/team/${t.key}`} className={styles.team}>
                  <span className={styles.teamKey}>{t.key}</span>
                  <span>{t.name}</span>
                </Link>
              </li>
            ))}
          </ul>
        )}
        {archived && (
          <div className={styles.archived} role="status">
            This project is archived.
          </div>
        )}
      </div>
      {editing && <ProjectFormDialog project={p} onClose={() => setEditing(false)} />}
      {deleting && <DeleteProjectDialog project={p} onClose={() => setDeleting(false)} />}
      <ProjectLogoField project={p} />
      <ProjectEmojiField project={p} />
      <Milestones id={p.id} />
      <ProjectUpdates project={p} canPost={!archived} />
      <IssueBrowser
        filters={filters}
        onChange={remembered.setFilters}
        onResetView={remembered.customised ? remembered.resetView : undefined}
        columns={remembered.columns}
        onColumnsChange={remembered.setColumns}
        extra={extra}
        lockProduct
        lockProject
        scopeTeams={keys.length > 0 ? keys : undefined}
        emptyTitle="No issues in this project yet"
        emptyNone={
          open && keys.length > 0 ? (
            <button type="button" className={`${ui.btn} ${ui.primary}`} onClick={create}>
              Create the first issue
            </button>
          ) : undefined
        }
      />
    </div>
  );
}

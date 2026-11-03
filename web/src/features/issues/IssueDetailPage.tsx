import { useCallback, useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link, useLocation, useParams } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import {
  useCreateLabel,
  useCycles,
  useIssue,
  useLabels,
  useMembers,
  useProject,
  useProjects,
  useSubIssues,
  useTeams,
  useTeamStates,
  useUpdateIssue,
} from '../../api/queries';
import { ApiError, errorMessage, isConflict } from '../../api/errors';
import type { Issue, UpdateIssueInput } from '../../api/types';
import { clip, usePageTitle } from '../../lib/documentTitle';
import { useGoBack } from '../../lib/backNav';
import { useHotkeys } from '../../lib/hotkeys';
import { Markdown } from '../../lib/markdown';
import { PRIORITIES, priorityLabel } from '../../lib/priority';
import { Avatar } from '../../ui/Avatar';
import { Logo } from '../../ui/Logo';
import { Breadcrumbs } from '../../ui/Breadcrumbs';
import { PriorityIcon, StatusIcon } from '../../ui/Icons';
import { PickerPopover } from '../../ui/Picker';
import { EmptyState, ErrorState, Spinner } from '../../ui/States';
import { useToast } from '../../ui/Toast';
import { useUi } from '../shell/ui-context';
import { useIssueActions } from './actions';
import { ActivityStream } from './Activity';
import { cycleName, sectionCycles } from '../cycles/cycles';
import { IssueSearchPicker } from './IssueSearchPicker';
import { AttachmentsArea } from './AttachmentPreview';
import { useIssueFileDrop } from './useIssueFileDrop';
import { DropOverlay } from '../../ui/useFileDrop';
import { LabelsField } from './LabelsField';
import { QuickPicker, type QuickKind } from './QuickPicker';
import { RelationsSection } from './Relations';
import { ChangeRequests } from './ChangeRequests';
import { DeleteIssueDialog } from './DeleteIssueDialog';
import { useIsOrgAdmin } from '../settings/access';
import ui from '../../styles/ui.module.css';
import styles from './issues.module.css';
import detail from './detail.module.css';

function SubIssues({ issue }: { issue: Issue }) {
  const org = useOrgSlug();
  const { openCreate } = useUi();
  const q = useSubIssues(issue.identifier);
  return (
    <section aria-label="Sub-issues" className={detail.sub}>
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load sub-issues" />
      ) : (
        q.data &&
        q.data.length > 0 && (
          <ul className={styles.relList}>
            {q.data.map((s) => (
              <li key={s.id} className={styles.relRow}>
                <StatusIcon type={s.state.type} color={s.state.color} name={s.state.name} />
                <Link to={`/${org}/issue/${s.identifier}`} state={{ issue: s }} className={styles.relLink}>
                  <span className={styles.ident}>{s.identifier}</span> {s.title}
                </Link>
                {s.assignee && <Avatar name={s.assignee.name} username={s.assignee.username} />}
              </li>
            ))}
          </ul>
        )
      )}
      <button
        type="button"
        className={`${ui.btn} ${ui.ghost} ${detail.addSub}`}
        onClick={() => openCreate({ team: issue.team.key, parent: issue.identifier })}
      >
        + Add sub-issues
      </button>
    </section>
  );
}

/** "Sub-issue of ORB-1 Title" with a done/total pill (completed sub-issues of the parent). */
function ParentLine({ issue }: { issue: Issue }) {
  const org = useOrgSlug();
  const parent = issue.parent!;
  const siblings = useSubIssues(parent.identifier);
  const total = siblings.data?.length ?? 0;
  const done = siblings.data?.filter((s) => s.state.type === 'completed').length ?? 0;
  return (
    <p className={detail.parentLine}>
      <span>Sub-issue of</span>
      <Link to={`/${org}/issue/${parent.identifier}`}>
        {parent.identifier} {parent.title}
      </Link>
      {total > 0 && (
        <span className={detail.pill} title={`${done} of ${total} sub-issues done`}>
          {done}/{total}
        </span>
      )}
    </p>
  );
}

export function IssueDetailPage() {
  const org = useOrgSlug();
  const { identifier = '' } = useParams();
  const loc = useLocation();
  const toast = useToast();
  // The copy the list passed in the location shows only until the first answer for this identifier (after that, also
  // after an `access` reset, it is never shown again), and only when its team is in the current team list: the
  // location state outlives the page (Back, a reload), and the team may be hidden now (T20 #72). With the team list
  // not loaded yet, no copy.
  const [answeredFor, setAnsweredFor] = useState<string | null>(null);
  const teams = useTeams();
  const statePlaceholder = (loc.state as { issue?: Issue } | null)?.issue;
  const placeholderTeamVisible =
    !!statePlaceholder && !!teams.data?.some((t) => t.key === statePlaceholder.team.key);
  const placeholder =
    answeredFor === identifier ||
    !placeholderTeamVisible ||
    statePlaceholder?.identifier.toUpperCase() !== identifier.toUpperCase()
      ? undefined
      : statePlaceholder;
  const q = useIssue(identifier, placeholder);
  useEffect(() => {
    if ((q.isSuccess && !q.isPlaceholderData) || q.isError) setAnsweredFor(identifier);
  }, [q.isSuccess, q.isPlaceholderData, q.isError, identifier]);
  const update = useUpdateIssue();
  const [conflict, setConflict] = useState(false);
  const onConflict = useCallback(() => setConflict(true), []);
  const actions = useIssueActions(onConflict);
  const members = useMembers();
  const createLabel = useCreateLabel();
  const issue = q.data;
  // The issue's team scopes the picker: org labels plus the team's own, with the team's counts.
  const labels = useLabels(issue?.team.key, !!issue);
  // Back and Escape share this: history back, or the team's issue list when the app has no earlier entry.
  const goBack = useGoBack(issue ? `/${org}/team/${issue.team.key}` : `/${org}/issues`);
  const drop = useIssueFileDrop(issue && !q.isPlaceholderData ? issue.identifier : undefined);
  const is404 = q.isError && !q.isPlaceholderData && q.error instanceof ApiError && q.error.status === 404;
  // A 404 for a cached issue whose team the person no longer sees (T20 #72) is not a delete: it is Not found, and
  // nothing of the cached copy (identifier, title, team) may stay on screen or in the title. The team list must be
  // fresh (not fetching), so a removal that has not reached it yet does not show the cached copy either.
  const hiddenNow =
    is404 &&
    !!issue &&
    (!teams.isSuccess || teams.isFetching || !teams.data.some((t) => t.key === issue.team.key));
  const notFound = is404 && (!issue || hiddenNow);
  // The title of the item already loaded; while it loads, the identifier only. Not the unsaved draft.
  usePageTitle(
    notFound
      ? 'Not found'
      : issue
        ? [issue.identifier, clip(issue.title)].filter(Boolean).join(' ')
        : identifier.slice(0, 30),
  );
  const states = useTeamStates(issue?.team.key);
  const teamRow = teams.data?.find((t) => t.key === issue?.team.key);
  const cyclesOn = !!teamRow?.cyclesEnabled;
  const cycles = useCycles(issue?.team.key, cyclesOn);
  const projects = useProjects(issue?.team.key);
  const listedProject = (projects.data ?? []).find((p) => p.id === issue?.projectId);
  // An archived project is not in the list: ask for it by id so the row still shows its name.
  const lone = useProject(issue?.projectId, projects.isSuccess && !listedProject);
  const qc = useQueryClient();
  const descRef = useRef<HTMLDivElement>(null);
  const wasEditing = useRef(false);
  const toggling = useRef(false);
  const [titleDraft, setTitleDraft] = useState<string | null>(null);
  const [descDraft, setDescDraft] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [quick, setQuick] = useState<QuickKind | null>(null);
  const [labelReq, setLabelReq] = useState(0);
  const loading = q.isPlaceholderData;
  const isAdmin = useIsOrgAdmin();
  const [deleting, setDeleting] = useState(false);

  // A different issue: forget drafts and any conflict.
  useEffect(() => {
    setTitleDraft(null);
    setDescDraft(null);
    wasEditing.current = false; // no focus jump to the next issue's description
    setConflict(false);
    setSaveError(null);
  }, [identifier]);

  // When the description editor closes (Save, Cancel, Escape) focus goes back to the text, not to the page.
  useEffect(() => {
    if (descDraft !== null) wasEditing.current = true;
    else if (wasEditing.current) {
      wasEditing.current = false;
      descRef.current?.focus();
    }
  }, [descDraft]);

  useHotkeys({
    s: () => issue && setQuick('status'),
    a: () => issue && setQuick('assignee'),
    l: () => issue && setLabelReq((n) => n + 1),
    p: () => issue && setQuick('priority'),
    Escape: goBack,
  });

  // The issue version each draft started from (see UpdateVars.baseVersion).
  const titleBase = useRef<number | undefined>(undefined);
  const descBase = useRef<number | undefined>(undefined);

  async function save(
    patch: Omit<UpdateIssueInput, 'version'>,
    optimistic?: Partial<Issue>,
    baseVersion?: number,
  ): Promise<boolean> {
    if (!issue) return false;
    setSaveError(null);
    try {
      await update.mutateAsync({ issue, patch, optimistic, baseVersion });
      return true;
    } catch (e) {
      if (isConflict(e)) setConflict(true);
      else setSaveError(errorMessage(e));
      return false;
    }
  }

  async function reload() {
    await q.refetch();
    setTitleDraft(null);
    setDescDraft(null);
    setConflict(false);
    setSaveError(null);
  }

  // The page was open when someone deleted the issue (LAUN-201): the event refetched it and it answered 404, while
  // the last good copy is still in the cache. Say so once, instead of a page whose every save fails.
  if (notFound) {
    return (
      <EmptyState title="Issue not found">
        <Link to={`/${org}/issues`}>Back to all issues</Link>
      </EmptyState>
    );
  }
  if (is404 && issue) {
    return (
      <EmptyState title="This issue was deleted">
        <p>{issue.identifier} no longer exists. It may have been deleted by someone else.</p>
        <Link to={`/${org}/team/${issue.team.key}`}>Back to {issue.team.name}</Link>
      </EmptyState>
    );
  }
  if (q.isError && !issue) {
    if (q.error instanceof ApiError && q.error.status === 404) {
      return (
        <EmptyState title="Issue not found">
          <Link to={`/${org}/issues`}>Back to all issues</Link>
        </EmptyState>
      );
    }
    return <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not load this issue" />;
  }
  if (!issue) return <Spinner label="Loading issue" />;

  const title = titleDraft ?? issue.title;
  const statesData = states.data ?? [];
  const sections = sectionCycles(cycles.data ?? []);
  const cycleChoices = [...sections.current, ...sections.next, ...sections.past.slice(0, 10)];
  const issueCycle = (cycles.data ?? []).find((c) => c.id === issue.cycleId);
  const project = listedProject ?? lone.data;

  function copy(text: string, done: string) {
    if (!navigator.clipboard) {
      toast.show('Could not copy', { kind: 'error' });
      return;
    }
    void navigator.clipboard.writeText(text).then(
      () => toast.show(done),
      () => toast.show('Could not copy', { kind: 'error' }),
    );
  }

  return (
    // The drop zone is the pane (it does not scroll); the scroller is inside it. An overlay inside the
    // scroller would scroll away and cover only the first screen (LAUN-105).
    <div className={ui.dropPane} {...drop.zoneProps}>
      {drop.active && <DropOverlay target={issue.identifier} />}
      <div className={detail.detail}>
        <div className={detail.bar}>
          <div className={detail.barLeft}>
            <button
              type="button"
              className={`${ui.btn} ${ui.ghost} ${detail.backBtn}`}
              aria-label="Back"
              onClick={goBack}
            >
              <span aria-hidden="true">&larr;</span> Back
            </button>
            <Breadcrumbs
              className={detail.crumbs}
              items={[
                {
                  key: 'team',
                  label: issue.team.name,
                  to: `/${org}/team/${issue.team.key}`,
                  title: `${issue.team.name} (${issue.team.key})`,
                },
                ...(issue.parent
                  ? [
                      {
                        key: 'parent',
                        label: issue.parent.identifier,
                        to: `/${org}/issue/${issue.parent.identifier}`,
                        title: `${issue.parent.identifier} ${issue.parent.title}`,
                      },
                    ]
                  : []),
                { key: 'issue', label: issue.identifier, title: `${issue.identifier} ${issue.title}` },
              ]}
            />
          </div>
          <div className={detail.toolbar} role="group" aria-label="Issue actions">
            <button
              type="button"
              className={`${ui.btn} ${ui.ghost} ${detail.toolBtn}`}
              onClick={() =>
                copy(`${window.location.origin}/${org}/issue/${issue.identifier}`, 'Link copied')
              }
            >
              Copy link
            </button>
            <button
              type="button"
              className={`${ui.btn} ${ui.ghost} ${detail.toolBtn}`}
              onClick={() => copy(issue.identifier, 'ID copied')}
            >
              Copy ID
            </button>
            <button
              type="button"
              className={`${ui.btn} ${ui.ghost} ${detail.toolBtn}`}
              title={issue.gitBranchName}
              onClick={() => copy(issue.gitBranchName, 'Branch name copied')}
            >
              Copy branch name
            </button>
            {isAdmin && (
              <button
                type="button"
                className={`${ui.btn} ${ui.ghost} ${detail.toolBtn}`}
                disabled={loading}
                onClick={() => setDeleting(true)}
              >
                Delete issue
              </button>
            )}
          </div>
        </div>

        <div className={detail.head}>
          {conflict && (
            <div className={ui.banner} role="alert">
              <strong>This issue changed elsewhere.</strong>
              <span>Your change was not saved, so nothing was overwritten.</span>
              <button type="button" className={`${ui.btn} ${ui.primary}`} onClick={() => void reload()}>
                Reload latest
              </button>
              {(titleDraft !== null || descDraft !== null) && (
                <details style={{ flexBasis: '100%' }}>
                  <summary>Your unsaved text</summary>
                  <pre style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
                    {[titleDraft, descDraft].filter((x) => x !== null).join('\n\n')}
                  </pre>
                </details>
              )}
            </div>
          )}
          {saveError && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {saveError}
            </p>
          )}
          <label className="sr-only" htmlFor="issue-title">
            Title
          </label>
          <textarea
            id="issue-title"
            className={detail.title}
            rows={1}
            value={title}
            disabled={loading}
            maxLength={500}
            onChange={(e) => {
              if (titleDraft === null) titleBase.current = issue.version;
              setTitleDraft(e.target.value.replace(/\n/g, ' '));
            }}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault();
                (e.target as HTMLTextAreaElement).blur();
              }
              if (e.key === 'Escape') {
                e.stopPropagation();
                setTitleDraft(null);
                (e.target as HTMLTextAreaElement).blur();
              }
            }}
            onBlur={async () => {
              if (titleDraft === null) return;
              const t = titleDraft.trim();
              if (!t || t === issue.title) return setTitleDraft(null);
              if (await save({ title: t }, { title: t }, titleBase.current)) setTitleDraft(null);
            }}
          />
          {issue.parent && <ParentLine issue={issue} />}
        </div>

        <div className={detail.main}>
          {descDraft !== null ? (
            <form
              className={`${detail.descEdit} ${detail.inputCard}`}
              onSubmit={async (e) => {
                e.preventDefault();
                if (await save({ description: descDraft }, { description: descDraft }, descBase.current))
                  setDescDraft(null);
              }}
            >
              <label className="sr-only" htmlFor="issue-desc">
                Description (markdown)
              </label>
              <textarea
                id="issue-desc"
                className={ui.textarea}
                style={{ minHeight: 180 }}
                value={descDraft}
                autoFocus
                onChange={(e) => setDescDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                    e.preventDefault();
                    e.currentTarget.form?.requestSubmit();
                  }
                  if (e.key === 'Escape') {
                    e.stopPropagation();
                    setDescDraft(null);
                  }
                }}
              />
              <div className={styles.composeBtns}>
                <button type="button" className={ui.btn} onClick={() => setDescDraft(null)}>
                  Cancel
                </button>
                <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={update.isPending}>
                  {update.isPending ? 'Saving…' : 'Save'}
                </button>
              </div>
            </form>
          ) : (
            // Edited in place: a click (or Enter) on the text turns it into the editor. Links stay links.
            <div
              ref={descRef}
              className={`${detail.desc} ${issue.description ? '' : detail.descEmpty}`}
              role="group"
              tabIndex={loading ? -1 : 0}
              aria-label="Description. Press Enter to edit."
              onClick={(e) => {
                if (loading || (e.target as HTMLElement).closest('a,button,img,input')) return;
                // Selecting text to copy it is not a request to edit.
                const sel = window.getSelection();
                if (sel && !sel.isCollapsed && sel.toString() !== '') return;
                descBase.current = issue.version;
                setDescDraft(issue.description);
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && e.target === e.currentTarget && !loading) {
                  e.preventDefault();
                  descBase.current = issue.version;
                  setDescDraft(issue.description);
                }
              }}
            >
              {issue.description ? (
                <Markdown
                  text={issue.description}
                  onToggleTask={
                    loading
                      ? undefined
                      : (d) => {
                          // One toggle at a time: a second would start from text that lacks the first.
                          if (toggling.current) return toast.show('Still saving the last change');
                          toggling.current = true;
                          void save({ description: d }, { description: d }, issue.version).finally(() => {
                            toggling.current = false;
                          });
                        }
                  }
                />
              ) : (
                'Add description…'
              )}
            </div>
          )}

          <SubIssues issue={issue} />
          <ChangeRequests items={issue.changeRequests} />
          <AttachmentsArea items={issue.attachments} description={issue.description} />
          <ActivityStream issue={issue} />
        </div>

        <aside className={detail.props} aria-label="Properties">
          <div className={detail.propSection}>
            <h2 className={detail.sectionTitle}>Properties</h2>
            <PickerPopover
              ariaLabel="Status"
              triggerLabel="Status"
              triggerClassName={detail.ghostBtn}
              display={
                <>
                  <StatusIcon type={issue.state.type} color={issue.state.color} name={issue.state.name} />{' '}
                  {issue.state.name}
                </>
              }
              options={statesData.map((s) => ({
                id: s.name,
                label: s.name,
                icon: <StatusIcon type={s.type} color={s.color} name={s.name} />,
              }))}
              selected={[issue.state.name]}
              onSelect={(o) => void actions.setState(issue, o.id)}
            />
            <PickerPopover
              ariaLabel="Priority"
              triggerLabel="Priority"
              triggerClassName={detail.ghostBtn}
              display={
                <>
                  <PriorityIcon priority={issue.priority} />{' '}
                  {issue.priority ? priorityLabel(issue.priority) : 'Set priority'}
                </>
              }
              options={PRIORITIES.map((p) => ({
                id: String(p.value),
                label: p.label,
                icon: <PriorityIcon priority={p.value} />,
              }))}
              selected={[String(issue.priority)]}
              onSelect={(o) => void actions.setPriority(issue, Number(o.id))}
            />
            <PickerPopover
              ariaLabel="Assignee"
              triggerLabel="Assignee"
              triggerClassName={detail.ghostBtn}
              display={
                issue.assignee ? (
                  <>
                    <Avatar name={issue.assignee.name} username={issue.assignee.username} />{' '}
                    {issue.assignee.name}
                  </>
                ) : (
                  'Assign'
                )
              }
              options={[
                { id: '', label: 'Unassigned' },
                ...(members.data ?? []).map((m) => ({
                  id: m.username,
                  label: m.name,
                  hint: m.username,
                  icon: <Avatar name={m.name} username={m.username} />,
                })),
              ]}
              selected={[issue.assignee?.username ?? '']}
              onSelect={(o) =>
                void actions.setAssignee(issue, members.data?.find((m) => m.username === o.id) ?? null)
              }
            />
            {cyclesOn && (
              <PickerPopover
                ariaLabel="Cycle"
                triggerLabel="Cycle"
                triggerClassName={detail.ghostBtn}
                display={issueCycle ? cycleName(issueCycle) : issue.cycleId ? 'Cycle' : 'Add to cycle'}
                options={[
                  { id: '', label: 'No cycle' },
                  ...cycleChoices.map((c) => ({ id: c.id, label: cycleName(c) })),
                ]}
                selected={[issue.cycleId ?? '']}
                onSelect={(o) =>
                  void actions
                    .run(issue, { cycleId: o.id || null }, { cycleId: o.id || null })
                    .then(() => qc.invalidateQueries({ queryKey: ['org', org, 'cycle-progress'] }))
                }
              />
            )}
          </div>

          <section aria-labelledby="labels-h" className={detail.propSection}>
            <h2 id="labels-h" className={detail.sectionTitle}>
              Labels
            </h2>
            <LabelsField
              issue={issue}
              all={labels.data ?? []}
              headingId="labels-h"
              openRequest={labelReq}
              onAdd={(l) => void actions.addLabel(issue, l)}
              onRemove={(l) => void actions.removeLabel(issue, l)}
              onCreate={(name) =>
                createLabel.mutate(
                  { name, team: issue.team.key },
                  {
                    onSuccess: (l) => void actions.addLabel(issue, l),
                    onError: (e) => toast.show(errorMessage(e), { kind: 'error' }),
                  },
                )
              }
            />
          </section>

          <section aria-labelledby="project-h" className={detail.propSection}>
            <h2 id="project-h" className={detail.sectionTitle}>
              Project
            </h2>
            <PickerPopover
              ariaLabel="Project"
              triggerLabel="Project"
              triggerClassName={detail.ghostBtn}
              display={
                project ? (
                  <>
                    <Logo
                      name={project.name}
                      src={project.avatarUrl}
                      emoji={project.emoji}
                      size={16}
                      decorative
                    />
                    {project.name}
                  </>
                ) : issue.projectId ? (
                  'Project'
                ) : (
                  'Add to project'
                )
              }
              options={[
                { id: '', label: 'No project' },
                ...(projects.data ?? []).map((p) => ({
                  id: p.id,
                  label: p.name,
                  icon: <Logo name={p.name} src={p.avatarUrl} emoji={p.emoji} size={16} decorative />,
                })),
              ]}
              selected={[issue.projectId ?? '']}
              onSelect={(o) =>
                void actions.run(
                  issue,
                  // A milestone belongs to the old project: clear it with the move.
                  { projectId: o.id || null, ...(issue.milestoneId ? { milestoneId: null } : {}) },
                  { projectId: o.id || null, ...(issue.milestoneId ? { milestoneId: null } : {}) },
                )
              }
            />
            {project && (
              <Link to={`/${org}/projects/${project.id}`} className={detail.projectLink}>
                Open project
              </Link>
            )}
          </section>

          <section aria-labelledby="parent-h" className={detail.propSection}>
            <h2 id="parent-h" className={detail.sectionTitle}>
              Parent
            </h2>
            <IssueSearchPicker
              ariaLabel="Parent issue"
              triggerLabel="Parent"
              triggerClassName={detail.ghostBtn}
              display={issue.parent ? <>{issue.parent.identifier}</> : 'Set parent'}
              allowNone
              exclude={[issue.identifier]}
              selected={issue.parent?.identifier}
              onSelect={(p) => void actions.run(issue, { parent: p ? p.identifier : null })}
            />
          </section>

          <RelationsSection issue={issue} />
        </aside>
        {deleting && <DeleteIssueDialog issue={issue} onClose={() => setDeleting(false)} />}
        {quick && (
          <QuickPicker kind={quick} targets={[issue]} actions={actions} onClose={() => setQuick(null)} />
        )}
      </div>
    </div>
  );
}

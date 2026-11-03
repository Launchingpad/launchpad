import { flushSync } from 'react-dom';
import { LabelDot } from '../../ui/LabelPill';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import {
  useCreateIssue,
  useAttachFiles,
  useCreateLabel,
  useCycles,
  useLabels,
  useMembers,
  useProjects,
  useTeamStates,
  useTeams,
} from '../../api/queries';
import { ApiError, errorMessage } from '../../api/errors';
import type { CreateIssueInput, Cycle, Issue, Label } from '../../api/types';
import { isTypingTarget } from '../../lib/hotkeys';
import { PRIORITIES } from '../../lib/priority';
import { uuid } from '../../lib/uuid';
import { ATTACH_TYPES, ATTACH_OFF, checkFiles } from '../../lib/attachRules';
import { DropOverlay, useFileDrop } from '../../ui/useFileDrop';
import { Avatar } from '../../ui/Avatar';
import { Logo } from '../../ui/Logo';
import { LabelList } from './LabelList';
import { ChipPicker } from '../../ui/ChipPicker';
import { Dialog } from '../../ui/Dialog';
import { Floating } from '../../ui/Floating';
import { PickerList } from '../../ui/Picker';
import {
  IconAttach,
  IconChevron,
  IconClose,
  IconCycle,
  IconExpand,
  IconMore,
  IconProject,
  IconTag,
  IconUser,
  PriorityIcon,
  StatusIcon,
} from '../../ui/Icons';
import { useToast } from '../../ui/Toast';
import { useUi } from '../shell/ui-context';
import { useIssueSearchOptions } from './IssueSearchPicker';
import { useIsOrgAdmin } from '../settings/access';
import { NO_TEAM_TEXT, NO_TEAM_TITLE } from './NoTeamAccess';
import ui from '../../styles/ui.module.css';

export function CreateIssueDialog() {
  const { create } = useUi();
  return create ? <Form key="create" /> : null;
}

/** The popovers the form can show. Only one is open at a time. */
type Pop = 'team' | 'status' | 'priority' | 'assignee' | 'labels' | 'project' | 'parent' | 'cycle' | 'more';
/** The shortcut letters: only while the dialog is open and no text field has focus. */
const SHORTCUTS: Record<string, Pop> = { s: 'status', p: 'priority', a: 'assignee', l: 'labels' };

const day = (iso: string) => new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
const cycleName = (c: Cycle) => c.name ?? `Cycle ${c.number}`;
/** The cycle that is running now: started, not ended, not completed. */
const currentCycle = (list: Cycle[], now = Date.now()) =>
  list.find((c) => !c.completedAt && Date.parse(c.startsAt) <= now && now < Date.parse(c.endsAt));

function Form() {
  const org = useOrgSlug();
  const toast = useToast();
  const { create, closeCreate, issueCreated } = useUi();
  const teams = useTeams();
  const isAdmin = useIsOrgAdmin();
  // decisions-T20 #73: no team to put an issue in. An admin sees every team (#2), so the org has none; a member
  // may lack access. The dialog stays open with the text and a disabled Create button.
  const noTeams = teams.isSuccess && teams.data.length === 0;
  const noTeamText = isAdmin
    ? 'This organisation has no team yet. An issue needs a team: create one in Settings, Teams.'
    : `${NO_TEAM_TITLE}. ${NO_TEAM_TEXT}`;
  const members = useMembers();
  const createLabel = useCreateLabel();
  const mutation = useCreateIssue();
  const attachFiles = useAttachFiles();
  const fileInput = useRef<HTMLInputElement>(null);
  const busyRef = useRef(false); // covers the create call AND the uploads
  const mountedRef = useRef(true);
  const createdRef = useRef<Issue | null>(null); // set when the create worked and files still wait
  const [busy, setBusy] = useState<'creating' | 'uploading' | null>(null);
  const [created, setCreated] = useState<string | null>(null);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);
  const key = useRef(uuid()); // one Idempotency-Key per issue, so a retry cannot create twice
  const titleRef = useRef<HTMLTextAreaElement>(null);

  const [team, setTeam] = useState<string | undefined>(
    (create?.team ?? create?.firstTeams?.[0])?.toUpperCase(),
  );
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [state, setState] = useState<string>(create?.state ?? '');
  const [priority, setPriority] = useState(0);
  const [assignee, setAssignee] = useState<string>(create?.assignee ?? '');
  const [labelNames, setLabelNames] = useState<string[]>([]);
  const [parent, setParent] = useState<string>(create?.parent ?? '');
  const [cycle, setCycle] = useState('');
  const cycleTouched = useRef(false); // once the person chooses (even "No cycle"), no preselect
  const [project, setProject] = useState('');
  const [files, setFiles] = useState<File[]>([]);
  const filesRef = useRef(files);
  filesRef.current = files;
  const lockRef = useRef<HTMLDivElement>(null);
  const [dueDate, setDueDate] = useState('');
  const [estimate, setEstimate] = useState('');
  const [error, setError] = useState<string | null>(null);
  // The same news as the toast, inside the dialog node (see `data-live-region` in Toast.tsx).
  const [announce, setAnnounce] = useState('');
  const [more, setMore] = useState(false); // "Create more"
  const [wide, setWide] = useState(false);
  const [pop, setPop] = useState<Pop | null>(null);
  const popRef = useRef(pop);
  popRef.current = pop;
  const [moreEl, setMoreEl] = useState<HTMLButtonElement | null>(null);

  // The team comes from the page or the product (a link, a remembered place); the list holds only the teams the
  // caller sees (T20 #71). A team that is not in it (hidden or deleted, also while the dialog is open after an
  // `access` event) is replaced by the first visible one, or none.
  useEffect(() => {
    if (!teams.data || created) return;
    if (team && teams.data.some((t) => t.key === team)) return;
    const next = teams.data[0]?.key;
    if (next === team) return;
    setTeam(next);
    if (team) {
      // States, cycles, projects and team labels belong to the old team. The parent came with it (a sub-issue of
      // an issue of that team) and may be hidden now: it goes too.
      setState('');
      setCycle('');
      setProject('');
      setLabelNames([]);
      setParent('');
      cycleTouched.current = false;
    }
  }, [team, teams.data, created]);
  // The product page lists its own teams first (LAUN-65, T18 #24); the rest keep their order after them.
  const teamOptions = useMemo(() => {
    const first = new Set((create?.firstTeams ?? []).map((k) => k.toUpperCase()));
    const all = (teams.data ?? []).map((t) => ({ id: t.key, label: t.name, hint: t.key }));
    return [
      ...all.filter((o) => first.has(o.id.toUpperCase())),
      ...all.filter((o) => !first.has(o.id.toUpperCase())),
    ];
  }, [teams.data, create?.firstTeams]);
  const teamRow = teams.data?.find((t) => t.key === team);
  const teamText = teamRow ? `${teamRow.key} · ${teamRow.name}` : (team ?? '…');
  const labels = useLabels(team, !!team);
  // The dialog picks labels by NAME and the server resolves a name to the team's label first (T21 #4), so an
  // org label and a team label of one name show once: the team's.
  const pickableLabels = useMemo(() => {
    const byName = new Map<string, Label>();
    for (const l of labels.data ?? []) {
      const k = l.name.toLowerCase();
      if (!byName.has(k) || l.teamId) byName.set(k, l);
    }
    return [...byName.values()];
  }, [labels.data]);
  const states = useTeamStates(team);
  const stateOptions = useMemo(() => states.data ?? [], [states.data]);
  const cycles = useCycles(team, !!teamRow?.cyclesEnabled);
  const projects = useProjects(team);

  useEffect(() => {
    if (noTeams) setAnnounce(noTeamText);
  }, [noTeams, noTeamText]);

  // The current cycle is preselected when the team has cycles on, until the person chooses.
  useEffect(() => {
    if (cycleTouched.current || cycle || !teamRow?.cyclesEnabled || !cycles.data) return;
    const cur = currentCycle(cycles.data);
    if (cur) setCycle(cur.id);
  }, [cycle, cycles.data, teamRow?.cyclesEnabled]);
  const parentSearch = useIssueSearchOptions({ allowNone: true, noneLabel: 'No parent' });

  // The parent search starts empty each time its popover opens (the list inside starts empty too).
  const resetParentQuery = parentSearch.onQuery;
  useEffect(() => {
    if (pop !== 'parent') resetParentQuery('');
  }, [pop, resetParentQuery]);

  // The "more" panel opens on its first field.
  useEffect(() => {
    if (pop === 'more') document.getElementById('ci-due')?.focus();
  }, [pop]);

  // S, P, A, L open a popover, like Linear. Not while a field has focus, not while one is open.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.isComposing || e.ctrlKey || e.metaKey || e.altKey) return;
      if (isTypingTarget(e.target) || popRef.current || createdRef.current) return;
      const target = SHORTCUTS[e.key.toLowerCase()];
      if (!target) return;
      e.preventDefault();
      setPop(target);
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, []);

  // The issue exists but files failed: the form is locked, so a close tells which files were not attached.
  const requestClose = () => {
    const issue = createdRef.current;
    if (issue && filesRef.current.length && !busyRef.current) {
      toast.show(
        `${issue.identifier} was created. Not attached: ${filesRef.current.map((f) => f.name).join(', ')}`,
      );
    }
    closeCreate();
  };

  const submit = useCallback(
    async (e?: React.FormEvent) => {
      e?.preventDefault();
      // One submit at a time, from the create call to the end of the uploads (button, Enter, Cmd+Enter).
      if (busyRef.current) return;
      let issue = createdRef.current;
      let input: CreateIssueInput | undefined;
      if (!issue) {
        if (!title.trim()) {
          setError('Add a title.');
          return;
        }
        if (noTeams) return;
        if (!team) {
          setError('Pick a team.');
          return;
        }
        const raw = estimate.trim();
        if (raw !== '' && !(/^\d+$/.test(raw) && Number(raw) <= 1000)) {
          setError('Estimate must be a whole number from 0 to 1000.');
          return;
        }
        const points = estimate.trim() === '' ? NaN : Number(estimate);
        input = {
          team,
          title: title.trim(),
          description: description.trim() || undefined,
          // A lane's state name (LAUN-173) may not exist in the team the person picks: send it only when it does.
          state: state && (!states.data || states.data.some((s) => s.name === state)) ? state : undefined,
          priority,
          assignee: assignee || undefined,
          labels: labelNames.length ? labelNames : undefined,
          parent: parent || undefined,
          projectId: project || undefined,
          cycleId: cycle || undefined,
          dueDate: dueDate || undefined,
          estimate: Number.isFinite(points) ? points : undefined,
        };
      }
      setError(null);
      busyRef.current = true;
      setBusy('creating');
      try {
        if (!issue && input) {
          issue = await mutation.mutateAsync({ idempotencyKey: key.current, input });
          // From here the issue exists: a retry only attaches files, it never creates again.
          createdRef.current = issue;
          issueCreated(issue); // LAUN-236: the board's or the list's undo can cancel it
          setCreated(issue.identifier);
          setPop(null);
          toast.show(`${issue.identifier} created`, {
            action: (
              <Link to={`/${org}/issue/${issue.identifier}`} state={{ issue }}>
                Open
              </Link>
            ),
          });
        }
        if (!issue) return;
        // Files wait for the issue: an upload needs an issue to attach to (T3b). The queue is
        // snapshotted and emptied first, so nothing can upload twice.
        const batch = files;
        let failed: File[] = [];
        const flags = { off: false };
        if (batch.length) {
          setBusy('uploading');
          if (mountedRef.current) setFiles((cur) => cur.filter((f) => !batch.includes(f)));
          failed = await attachFiles(issue.identifier, batch, {
            onFailed: (_f, err) => {
              if (err instanceof ApiError && err.code === 'attachments_unavailable') flags.off = true;
            },
          });
        }
        if (!mountedRef.current) {
          // The dialog was closed meanwhile: do not touch it or close a newer one.
          if (failed.length)
            toast.show(`${issue.identifier}: could not attach ${failed.map((f) => f.name).join(', ')}`);
          return;
        }
        if (failed.length) {
          // Keep the failed files, so the person sees them and can retry or remove them. The dialog
          // stays open, also with Create more on.
          setFiles((cur) => [...failed, ...cur.filter((f) => !batch.includes(f))]);
          setError(
            `${issue.identifier} was created. Could not attach: ${failed.map((f) => f.name).join(', ')}. The issue is locked; Retry attach tries these files only.${flags.off ? ` ${ATTACH_OFF}` : ''}`,
          );
          setAnnounce(`Created ${issue.identifier}. Some files were not attached.`);
          return;
        }
        createdRef.current = null;
        setCreated(null);
        setAnnounce(`Created ${issue.identifier}.${more ? ' The dialog is open for the next issue.' : ''}`);
        if (more) {
          // Stay open: the text goes, the chosen properties stay. A new key makes the next
          // create a new issue (the old key would replay this one).
          key.current = uuid();
          setTitle('');
          setDescription('');
          // Unmount the panel before focusing the title, so a queued blur of the panel cannot take focus.
          flushSync(() => setPop(null));
          titleRef.current?.focus();
        } else {
          closeCreate();
        }
      } catch (err) {
        if (mountedRef.current) setError(errorMessage(err));
        else toast.show(`Could not create "${title.trim()}": ${errorMessage(err)}`);
        // The server keeps a 4xx answer under its key: the same key with the same body replays that
        // answer, with another body it is a 409. So after a refusal the corrected form needs a new key.
        // A network error, a 5xx or an unknown outcome keeps the key, so a retry cannot make two issues.
        if (
          err instanceof ApiError &&
          err.status >= 400 &&
          err.status < 500 &&
          err.code !== 'idempotency_in_progress' &&
          err.code !== 'idempotency_outcome_unknown'
        ) {
          key.current = uuid();
        }
      } finally {
        busyRef.current = false;
        if (mountedRef.current) setBusy(null);
      }
    },
    [
      mutation,
      noTeams,
      title,
      team,
      estimate,
      description,
      state,
      states.data,
      priority,
      assignee,
      labelNames,
      parent,
      cycle,
      project,
      files,
      attachFiles,
      dueDate,
      more,
      toast,
      org,
      closeCreate,
      issueCreated,
    ],
  );

  // Once the issue exists the properties cannot change (a retry only attaches files).
  useEffect(() => {
    const el = lockRef.current;
    if (!el) return;
    if (created) {
      // Focus inside the row would fall to <body> when the row turns inert, and the dialog would stop
      // seeing keys (Esc, Tab trap, Cmd+Enter). Move it to the title first.
      if (el.contains(document.activeElement)) titleRef.current?.focus();
      el.setAttribute('inert', '');
    } else el.removeAttribute('inert');
  }, [created]);

  const onModEnter = (e: React.KeyboardEvent) => {
    if (e.nativeEvent.isComposing || e.keyCode === 229) return;
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void submit();
    }
  };

  const sel =
    stateOptions.find((s) => s.name === state) ??
    stateOptions.find((s) => s.id === teamRow?.defaultStateId) ??
    stateOptions[0];
  const member = assignee ? members.data?.find((m) => m.username === assignee) : undefined;
  const cycleRow = cycles.data?.find((c) => c.id === cycle);
  const projectRow = projects.data?.find((c) => c.id === project);
  // Running and coming cycles, and the last two finished ones.
  const cycleOptions = (() => {
    const all = cycles.data ?? [];
    const live = all.filter((c) => !c.completedAt).sort((a, b) => a.startsAt.localeCompare(b.startsAt));
    const past = all
      .filter((c) => c.completedAt)
      .sort((a, b) => b.startsAt.localeCompare(a.startsAt))
      .slice(0, 2)
      .reverse();
    return [...past, ...live];
  })();
  // The paperclip and a file drop share this: check the files, queue the good ones, name the others.
  function addPicked(picked: File[]) {
    if (busyRef.current || !picked.length) return;
    const { ok, refused } = checkFiles(picked);
    if (refused) setError(refused);
    if (ok.length) setFiles((cur) => [...cur, ...ok]);
    setAnnounce(
      [
        refused,
        ok.length &&
          `${ok.length === 1 ? `${ok[0]!.name} is` : `${ok.length} files are`} ready to attach when the issue is created.`,
      ]
        .filter(Boolean)
        .join(' '),
    );
  }
  const drop = useFileDrop((dropped) => addPicked(dropped), busy !== null);
  const open = (p: Pop) => ({ open: pop === p, onOpenChange: (v: boolean) => setPop(v ? p : null) });
  const extras = [
    dueDate && `due ${dueDate}`,
    estimate.trim() && `estimate ${estimate.trim()}`,
    parent && `parent ${parent}`,
  ].filter(Boolean);

  return (
    <Dialog
      title="New issue"
      onClose={requestClose}
      onKey={onModEnter}
      width={wide ? 900 : 640}
      plain
      panelProps={drop.zoneProps}
      overlay={drop.active ? <DropOverlay /> : undefined}
      header={
        <div className={ui.createHead}>
          {(teams.data?.length ?? 0) > 1 ? (
            <ChipPicker
              label="Team"
              disabled={!!created}
              display={<span>{teamText}</span>}
              valueText={teamRow ? `${teamRow.key} ${teamRow.name}` : undefined}
              placeholder="Change team…"
              options={teamOptions}
              selected={team ? [team] : []}
              onSelect={(o) => {
                setTeam(o.id);
                // States and cycles belong to a team.
                setState('');
                setCycle('');
                setProject('');
                // Team labels belong to a team: the new team's list may not have them (LAUN-101).
                setLabelNames([]);
                cycleTouched.current = false;
              }}
              {...open('team')}
            />
          ) : (
            <span className={`${ui.chipBtn} ${ui.chipStatic} ${ui.createTeam}`} title={teamRow?.name}>
              <span>{teamText}</span>
            </span>
          )}
          <span aria-hidden="true" className={ui.hint}>
            ›
          </span>
          <span className={ui.createHeadTitle}>New issue</span>
          <button
            type="button"
            className={`${ui.btn} ${ui.ghost} ${ui.icon}`}
            aria-label={wide ? 'Make the dialog smaller' : 'Make the dialog larger'}
            aria-pressed={wide}
            onClick={() => setWide((w) => !w)}
          >
            <IconExpand />
          </button>
          <button
            type="button"
            className={`${ui.btn} ${ui.ghost} ${ui.icon}`}
            aria-label="Close"
            onClick={requestClose}
          >
            <IconClose />
          </button>
        </div>
      }
      footer={
        <div className={ui.createFoot}>
          <input
            ref={fileInput}
            type="file"
            multiple
            hidden
            accept={ATTACH_TYPES.join(',')}
            data-testid="attach-input"
            onChange={(e) => {
              const picked = Array.from(e.target.files ?? []);
              e.target.value = '';
              addPicked(picked);
            }}
          />
          <button
            type="button"
            className={ui.attachBtn}
            aria-label="Attach files"
            title="Attach files (uploaded when the issue is created)"
            disabled={busy !== null}
            onClick={() => fileInput.current?.click()}
          >
            <IconAttach />
          </button>
          <div className={ui.createFootEnd}>
            <button
              type="button"
              role="switch"
              aria-checked={more}
              className={ui.switchBtn}
              onClick={() => setMore((m) => !m)}
            >
              <span className={ui.switchTrack} aria-hidden="true" />
              Create more
            </button>
            <button
              type="submit"
              form="create-issue"
              className={`${ui.btn} ${ui.primary}`}
              disabled={busy !== null || (noTeams && !created)}
              aria-describedby={noTeams && !created ? 'create-issue-no-team' : undefined}
            >
              {busy === 'creating'
                ? 'Creating…'
                : busy === 'uploading'
                  ? 'Uploading…'
                  : created
                    ? 'Retry attach'
                    : 'Create issue'}
            </button>
          </div>
        </div>
      }
    >
      {noTeams && !created && (
        // Announced through the dialog's live region (below), which is in the page before the text comes.
        <p id="create-issue-no-team" className={`${ui.banner} ${ui.createNoTeam}`}>
          {noTeamText}
        </p>
      )}
      <form id="create-issue" onSubmit={submit}>
        {/* A one-line field that grows: a long title wraps and stays readable. Enter creates, as before. */}
        <textarea
          ref={titleRef}
          data-autofocus
          rows={1}
          aria-label="Title"
          className={`${ui.createTitle}`}
          readOnly={!!created}
          value={title}
          maxLength={500}
          onChange={(e) => setTitle(e.target.value.replace(/\s*\n+\s*/g, ' '))}
          onKeyDown={(e) => {
            if (
              e.key === 'Enter' &&
              !e.shiftKey &&
              !e.metaKey &&
              !e.ctrlKey &&
              !e.nativeEvent.isComposing &&
              e.keyCode !== 229
            ) {
              e.preventDefault();
              void submit();
            }
          }}
          placeholder="Issue title"
        />
        <textarea
          aria-label="Description"
          className={ui.createDesc}
          readOnly={!!created}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          placeholder="Add description…"
        />
        <div ref={lockRef} className={`${ui.chipRow} ${ui.createChips}`} role="group" aria-label="Properties">
          <ChipPicker
            label="Status"
            shortcut="S"
            display={
              <>
                {sel && <StatusIcon type={sel.type} color={sel.color} name={sel.name} />}
                <span>{sel?.name ?? 'Status'}</span>
              </>
            }
            valueText={sel?.name}
            unset={!sel}
            placeholder="Set status…"
            options={stateOptions.map((s) => ({
              id: s.name,
              label: s.name,
              icon: <StatusIcon type={s.type} color={s.color} name={s.name} />,
            }))}
            selected={sel ? [sel.name] : []}
            onSelect={(o) => setState(o.id)}
            {...open('status')}
          />
          <ChipPicker
            label="Priority"
            shortcut="P"
            display={
              <>
                <PriorityIcon priority={priority} />
                <span>
                  {priority ? (PRIORITIES.find((p) => p.value === priority)?.label ?? '') : 'Priority'}
                </span>
              </>
            }
            valueText={priority ? PRIORITIES.find((p) => p.value === priority)?.label : undefined}
            unset={priority === 0}
            placeholder="Set priority…"
            options={PRIORITIES.map((p) => ({
              id: String(p.value),
              label: p.label,
              icon: <PriorityIcon priority={p.value} />,
            }))}
            selected={[String(priority)]}
            onSelect={(o) => setPriority(Number(o.id))}
            {...open('priority')}
          />
          <ChipPicker
            label="Assignee"
            shortcut="A"
            display={
              <>
                {assignee ? <Avatar name={member?.name ?? assignee} username={assignee} /> : <IconUser />}
                <span>{assignee ? (member?.name ?? assignee) : 'Assignee'}</span>
              </>
            }
            valueText={assignee ? (member?.name ?? assignee) : undefined}
            unset={!assignee}
            placeholder="Assign to…"
            options={[
              { id: '', label: 'No assignee', icon: <IconUser /> },
              ...(members.data ?? []).map((m) => ({
                id: m.username,
                label: m.name,
                hint: m.username,
                icon: <Avatar name={m.name} username={m.username} />,
              })),
            ]}
            selected={[assignee]}
            onSelect={(o) => setAssignee(o.id)}
            {...open('assignee')}
          />
          <ChipPicker
            label="Project"
            display={
              <>
                {projectRow ? (
                  <Logo
                    name={projectRow.name}
                    src={projectRow.avatarUrl}
                    emoji={projectRow.emoji}
                    size={16}
                    decorative
                  />
                ) : (
                  <IconProject />
                )}
                <span>{projectRow?.name ?? 'Project'}</span>
              </>
            }
            valueText={projectRow?.name}
            unset={!projectRow}
            placeholder="Set project…"
            options={[
              { id: '', label: 'No project', icon: <IconProject /> },
              ...(projects.data ?? [])
                .filter((p) => p.state !== 'completed' && p.state !== 'canceled')
                .map((p) => ({
                  id: p.id,
                  label: p.name,
                  icon: <Logo name={p.name} src={p.avatarUrl} emoji={p.emoji} size={16} decorative />,
                })),
            ]}
            selected={[project]}
            onSelect={(o) => setProject(o.id)}
            {...open('project')}
          />
          <ChipPicker
            label="Labels"
            shortcut="L"
            multi
            display={
              <>
                {labelNames.length ? (
                  labelNames
                    .slice(0, 3)
                    .map((n) => <LabelDot key={n} color={labels.data?.find((l) => l.name === n)?.color} />)
                ) : (
                  <IconTag />
                )}
                <span>
                  {labelNames.length === 0
                    ? 'Labels'
                    : labelNames.length === 1
                      ? labelNames[0]
                      : `${labelNames.length} labels`}
                </span>
              </>
            }
            valueText={labelNames.length ? labelNames.join(', ') : undefined}
            unset={labelNames.length === 0}
            placeholder="Add labels…"
            options={[]}
            renderList={(list) => (
              <LabelList
                list={list}
                labels={pickableLabels}
                idOf={(l) => l.name}
                projectId={project || null}
                parent={parent || null}
              />
            )}
            selected={labelNames}
            onSelect={(o) =>
              setLabelNames((n) => (n.includes(o.id) ? n.filter((x) => x !== o.id) : [...n, o.id]))
            }
            onCreate={(name) => {
              // No team yet: the label would be org-wide without anyone meaning it.
              if (!team) return;
              createLabel.mutate(
                { name, team },
                {
                  onSuccess: (l) => setLabelNames((n) => (n.includes(l.name) ? n : [...n, l.name])),
                  onError: (e) => toast.show(errorMessage(e), { kind: 'error' }),
                },
              );
            }}
            {...open('labels')}
          />
          {teamRow?.cyclesEnabled && (
            <ChipPicker
              label="Cycle"
              display={
                <>
                  <IconCycle />
                  <span>{cycleRow ? cycleName(cycleRow) : 'Cycle'}</span>
                </>
              }
              valueText={cycleRow ? cycleName(cycleRow) : undefined}
              unset={!cycleRow}
              placeholder="Set cycle…"
              options={[
                { id: '', label: 'No cycle', icon: <IconCycle /> },
                ...cycleOptions.map((c) => ({
                  id: c.id,
                  label: cycleName(c),
                  hint: `${day(c.startsAt)} – ${day(c.endsAt)}`,
                  icon: <IconCycle />,
                })),
              ]}
              selected={[cycle]}
              onSelect={(o) => {
                cycleTouched.current = true;
                setCycle(o.id);
              }}
              {...open('cycle')}
            />
          )}
          <button
            ref={setMoreEl}
            type="button"
            className={`${ui.chipBtn} ${extras.length ? '' : ui.chipUnset}`}
            aria-haspopup="dialog"
            aria-expanded={pop === 'more'}
            aria-label={extras.length ? `More options: ${extras.join(', ')}` : 'More options'}
            onMouseDown={(e) => {
              if (pop === 'more') e.preventDefault(); // focus must not fall to the dialog node
            }}
            onClick={() => {
              if (pop === 'more') moreEl?.focus(); // focus first, then close: focus must not drop to the body
              setPop(pop === 'more' ? null : 'more');
            }}
            onKeyDown={(e) => {
              if (pop === 'more' && e.key === 'Escape') {
                e.stopPropagation();
                e.preventDefault();
                setPop(null);
              }
            }}
          >
            <IconMore />
            {extras.length > 0 && <span>{extras.length}</span>}
          </button>
          {pop === 'more' && moreEl && (
            <Floating
              anchor={moreEl}
              label="More options"
              width={280}
              onClose={(restore) => {
                if (restore) moreEl.focus();
                setPop(null);
              }}
            >
              <div className={ui.moreForm} role="group" aria-label="More options">
                <div className={ui.field}>
                  <label htmlFor="ci-due">Due date</label>
                  <input
                    id="ci-due"
                    type="date"
                    className={ui.input}
                    value={dueDate}
                    onChange={(e) => setDueDate(e.target.value)}
                  />
                </div>
                <div className={ui.field}>
                  <label htmlFor="ci-est">Estimate (points)</label>
                  <input
                    id="ci-est"
                    type="number"
                    min={0}
                    max={1000}
                    step={1}
                    className={ui.input}
                    value={estimate}
                    onChange={(e) => setEstimate(e.target.value)}
                  />
                </div>
                <button
                  type="button"
                  className={ui.moreRow}
                  aria-haspopup="listbox"
                  onClick={() => setPop('parent')}
                >
                  <span>Parent issue{parent ? `: ${parent}` : ''}</span>
                  <IconChevron />
                </button>
                <button
                  type="button"
                  className={`${ui.btn} ${ui.ghost}`}
                  disabled={!dueDate && !estimate}
                  onClick={() => {
                    setDueDate('');
                    setEstimate('');
                    // Focus must not sit on a button that has just disabled itself.
                    document.getElementById('ci-due')?.focus();
                  }}
                >
                  Clear both
                </button>
              </div>
            </Floating>
          )}
        </div>
        {files.length > 0 && (
          <ul className={ui.attachList} aria-label="Files to attach">
            {files.map((f, i) => (
              <li key={`${f.name}-${i}`} className={ui.attachItem}>
                <span>{f.name}</span>
                <button
                  type="button"
                  className={ui.attachRemove}
                  aria-label={`Remove ${f.name}`}
                  disabled={busy !== null}
                  onClick={() => setFiles((cur) => cur.filter((_, j) => j !== i))}
                >
                  <IconClose width={12} height={12} />
                </button>
              </li>
            ))}
          </ul>
        )}
        {pop === 'parent' && moreEl && (
          <Floating
            anchor={moreEl}
            label="Parent issue"
            onClose={(restore) => {
              if (restore) moreEl.focus();
              setPop(null);
            }}
          >
            <PickerList
              ariaLabel="Parent issue"
              placeholder="Set parent…"
              remote
              options={parentSearch.options}
              onQuery={parentSearch.onQuery}
              loading={parentSearch.loading}
              emptyText={parentSearch.emptyText}
              selected={parent ? [parent] : []}
              onSelect={(o) => {
                setParent(o.id);
                moreEl.focus();
                setPop(null);
              }}
            />
          </Floating>
        )}
        <div className="sr-only" role="status" aria-live="polite" aria-atomic="true">
          {announce}
        </div>
        {error && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`} style={{ marginTop: 12 }}>
            {error}
          </p>
        )}
      </form>
    </Dialog>
  );
}

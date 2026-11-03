import { productTeamsTitle, teamKeysShort } from '../../ui/teamKeys';
import { useEffect, useId, useState, useSyncExternalStore, type ReactNode } from 'react';
import { useAllProjects } from '../projects/queries';
import { Logo } from '../../ui/Logo';
import { LabelDot } from '../../ui/LabelPill';
import { GROUP_KEYS, GROUP_NAMES, type GroupKey, type IssueFilters, type SortKey } from './filters';
import { activeFilterCount, emptyFilters } from './filters';
import { useAllStates, useLabels, useMembers, useProducts } from '../../api/queries';
import { PRIORITIES } from '../../lib/priority';
import { PickerPopover, type PickerOption } from '../../ui/Picker';
import { IconBoard, IconList, IconSearch } from '../../ui/Icons';
import { PriorityIcon, StatusIcon } from '../../ui/Icons';
import { Avatar } from '../../ui/Avatar';
import ui from '../../styles/ui.module.css';
import styles from './issues.module.css';

function summary(name: string, values: string[]): string {
  if (!values.length) return name;
  return values.length === 1 ? `${name}: ${values[0]}` : `${name}: ${values[0]} +${values.length - 1}`;
}

const NARROW = '(max-width: 760px)';
function subscribeNarrow(cb: () => void) {
  const m = window.matchMedia(NARROW);
  m.addEventListener('change', cb);
  return () => m.removeEventListener('change', cb);
}
function useNarrow(): boolean {
  return useSyncExternalStore(
    subscribeNarrow,
    () => window.matchMedia(NARROW).matches,
    () => false,
  );
}

export function FilterBar({
  filters,
  onChange,
  onResetView,
  lockAssignee,
  lockTeam,
  lockProduct,
  lockProject,
  scopeTeams,
  selectMode,
  onSelectMode,
  columnsControl,
}: {
  filters: IssueFilters;
  onChange: (f: IssueFilters) => void;
  /** LAUN-109: forget this place's remembered layout and sort. Absent while the view is the default. */
  onResetView?: () => void;
  lockAssignee?: boolean;
  lockTeam?: string;
  /** The product page fixes the product (it is in `extra`, not in the filters), so no Product chip. */
  lockProduct?: boolean;
  /** The project page fixes the project (LAUN-179), so no Project chip. */
  lockProject?: boolean;
  /** Team keys whose states the Status chip offers (the product page, LAUN-65). */
  scopeTeams?: string[];
  selectMode?: boolean;
  onSelectMode?: (on: boolean) => void;
  /** LAUN-163: the board's Columns control; the browser passes it only while the board shows. */
  columnsControl?: ReactNode;
}) {
  // Narrow screens fold the chips, Clear and Sort behind one Filter button (LAUN-16).
  // On a wide screen both folds are display: contents, so the bar is as it was.
  const [open, setOpen] = useState(false);
  // The DOM order is the focus order: on a phone the chips come after the Filter button.
  const narrow = useNarrow();
  const uid = useId();
  const chipsId = `${uid}-chips`;
  const moreId = `${uid}-more`;
  const { states, teams } = useAllStates();
  const members = useMembers();
  const products = useProducts();
  const projects = useAllProjects(!lockProject);
  const [q, setQ] = useState(filters.q);
  useEffect(() => setQ(filters.q), [filters.q]);
  useEffect(() => {
    if (q === filters.q) return;
    const t = setTimeout(() => onChange({ ...filters, q }), 250);
    return () => clearTimeout(t);
  }, [q, filters, onChange]);

  const teamKey = lockTeam ?? filters.team;
  // On a team page: the org labels and the team's (LAUN-101). `?label=` and `team=` go together.
  const labels = useLabels(teamKey);
  const teamId = teams.find((t) => t.key.toLowerCase() === teamKey?.toLowerCase())?.id;
  const scopeIds = scopeTeams
    ? new Set(
        teams.filter((t) => scopeTeams.some((k) => k.toLowerCase() === t.key.toLowerCase())).map((t) => t.id),
      )
    : null;
  const scoped = teamId
    ? states.filter((s) => s.teamId === teamId)
    : scopeIds
      ? states.filter((s) => scopeIds.has(s.teamId))
      : states;
  const stateOptions: PickerOption[] = [];
  for (const s of scoped)
    if (!stateOptions.some((o) => o.id === s.name))
      stateOptions.push({
        id: s.name,
        label: s.name,
        icon: <StatusIcon type={s.type} color={s.color} name={s.name} />,
      });

  const assigneeOptions: PickerOption[] = [
    { id: 'me', label: 'Me' },
    { id: 'none', label: 'Unassigned' },
    ...(members.data ?? []).map((m) => ({
      id: m.username,
      label: m.name,
      hint: m.username,
      icon: <Avatar name={m.name} username={m.username} />,
    })),
  ];
  // The filter matches by NAME (decisions-T21 #4), so a name shows once even when an org label and a team
  // label share it.
  const seen = new Set<string>();
  const labelOptions: PickerOption[] = [];
  const hasTeamLabels = (labels.data ?? []).some((l) => l.teamId);
  for (const l of [...(labels.data ?? [])].sort((a, b) => Number(!!a.teamId) - Number(!!b.teamId))) {
    if (seen.has(l.name.toLowerCase())) continue;
    seen.add(l.name.toLowerCase());
    labelOptions.push({
      id: l.name,
      label: l.name,
      icon: <LabelDot color={l.color} />,
      section: hasTeamLabels ? (l.teamId ? 'Team labels' : 'Org labels') : undefined,
    });
  }
  // LAUN-179: one project, or none (the server takes an id or `none`, as assignee takes `none`).
  const projectOptions: PickerOption[] = [
    { id: 'none', label: 'No project' },
    ...(projects.data ?? []).map((p) => ({
      id: p.id,
      label: p.name,
      icon: <Logo name={p.name} src={p.avatarUrl} emoji={p.emoji} size={16} decorative />,
    })),
  ];
  // An id the list does not hold (loading, deleted, archived) shows as the raw value, like the assignee chip.
  const projectLabel = filters.project
    ? (projectOptions.find((o) => o.id === filters.project)?.label ?? filters.project)
    : '';
  const assigneeLabel = filters.assignee
    ? (assigneeOptions.find((o) => o.id === filters.assignee)?.label ?? filters.assignee)
    : '';
  // The Product chip shows when the org has a product (T18 #32) or one is already in the URL, so it can be
  // cleared. A team page fixes the team, so a product would only narrow it (AND) and the chip is left out.
  const productOptions: PickerOption[] = (products.data ?? []).map((p) => ({
    id: p.slug,
    label: p.name,
    hint: teamKeysShort(p.teamKeys),
    title: productTeamsTitle(p.name, p.teamKeys),
    icon: p.icon ? <span aria-hidden="true">{p.icon}</span> : undefined,
  }));
  const productSel = filters.product
    ? (products.data ?? []).find((p) => p.slug === filters.product)
    : undefined;
  const productLabel = filters.product ? (productSel?.name ?? filters.product) : '';
  // LAUN-100: the chip names the product's teams too (two keys, then "+N"); the tooltip holds them all.
  const productDisplay = productLabel
    ? `Product: ${productLabel}${productSel ? ` (${teamKeysShort(productSel.teamKeys, 2)})` : ''}`
    : 'Product';
  const showProduct = !lockTeam && !lockProduct && (productOptions.length > 0 || !!filters.product);
  const toggle = (list: string[], v: string) =>
    list.includes(v) ? list.filter((x) => x !== v) : [...list, v];
  const n = activeFilterCount(filters);

  const chipsFold = (
    <div key="chips" className={styles.fold} id={chipsId}>
      {showProduct && (
        <PickerPopover
          ariaLabel="Filter by product"
          display={productDisplay}
          triggerTitle={productSel ? productTeamsTitle(productSel.name, productSel.teamKeys) : undefined}
          options={productOptions}
          selected={filters.product ? [filters.product] : []}
          onSelect={(o) => onChange({ ...filters, product: filters.product === o.id ? undefined : o.id })}
        />
      )}
      <PickerPopover
        ariaLabel="Filter by status"
        display={summary('Status', filters.state)}
        options={stateOptions}
        selected={filters.state}
        multi
        onSelect={(o) => onChange({ ...filters, state: toggle(filters.state, o.id) })}
      />
      {!lockAssignee && (
        <PickerPopover
          ariaLabel="Filter by assignee"
          display={assigneeLabel ? `Assignee: ${assigneeLabel}` : 'Assignee'}
          options={assigneeOptions}
          selected={filters.assignee ? [filters.assignee] : []}
          onSelect={(o) => onChange({ ...filters, assignee: filters.assignee === o.id ? undefined : o.id })}
        />
      )}
      {!lockProject && (
        <PickerPopover
          ariaLabel="Filter by project"
          display={projectLabel ? `Project: ${projectLabel}` : 'Project'}
          options={projectOptions}
          selected={filters.project ? [filters.project] : []}
          onSelect={(o) => onChange({ ...filters, project: filters.project === o.id ? undefined : o.id })}
        />
      )}
      <PickerPopover
        ariaLabel="Filter by label"
        display={summary('Label', filters.label)}
        options={labelOptions}
        selected={filters.label}
        multi
        onSelect={(o) => onChange({ ...filters, label: toggle(filters.label, o.id) })}
      />
      <PickerPopover
        ariaLabel="Filter by priority"
        display={
          filters.priority !== undefined
            ? `Priority: ${PRIORITIES.find((p) => p.value === filters.priority)?.label}`
            : 'Priority'
        }
        options={PRIORITIES.map((p) => ({
          id: String(p.value),
          label: p.label,
          icon: <PriorityIcon priority={p.value} />,
        }))}
        selected={filters.priority !== undefined ? [String(filters.priority)] : []}
        onSelect={(o) =>
          onChange({ ...filters, priority: filters.priority === Number(o.id) ? undefined : Number(o.id) })
        }
      />
    </div>
  );
  const moreFold = (
    <div key="more" className={styles.fold} id={moreId}>
      {onSelectMode && (
        <button
          type="button"
          className={`${ui.btn} ${styles.selectToggle}`}
          aria-pressed={!!selectMode}
          onClick={() => onSelectMode(!selectMode)}
        >
          Select
        </button>
      )}
      {n > 0 && (
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost}`}
          onClick={() =>
            onChange({
              ...emptyFilters,
              sort: filters.sort,
              view: filters.view,
              group: filters.group,
              team: filters.team,
            })
          }
        >
          Clear ({n})
        </button>
      )}
      <span className={styles.spacer} />
      {filters.view === 'board' && (
        <label className={styles.sort}>
          <span className="sr-only">Group by</span>
          <select
            className={ui.select}
            value={filters.group}
            title="Group the board's columns by"
            onChange={(e) => onChange({ ...filters, group: e.target.value as GroupKey })}
          >
            {GROUP_KEYS.map((g) => (
              <option key={g} value={g}>
                {g === 'state' ? 'Group: Status' : `Group: ${GROUP_NAMES[g]}`}
              </option>
            ))}
          </select>
        </label>
      )}
      {columnsControl}
      <label className={styles.sort}>
        <span className="sr-only">Sort by</span>
        <select
          className={ui.select}
          value={filters.sort}
          title={
            filters.sort === 'manual' ? 'Manual: drag cards on the board to change the order' : undefined
          }
          onChange={(e) => onChange({ ...filters, sort: e.target.value as SortKey })}
        >
          <option value="updated">Updated</option>
          <option value="created">Created</option>
          <option value="priority">Priority</option>
          <option value="manual">Manual</option>
        </select>
      </label>
      {onResetView && (
        <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onResetView}>
          Reset view
        </button>
      )}
    </div>
  );
  const viewToggle = (
    <div key="view" className={styles.viewToggle} role="group" aria-label="View">
      <button
        type="button"
        className={`${ui.btn} ${ui.icon}`}
        aria-pressed={filters.view === 'list'}
        aria-label="List view"
        onClick={() => onChange({ ...filters, view: 'list' })}
      >
        <IconList />
      </button>
      <button
        type="button"
        className={`${ui.btn} ${ui.icon}`}
        aria-pressed={filters.view === 'board'}
        aria-label="Board view"
        // LAUN-110: the board starts in Manual, the only order a card can be dragged in.
        onClick={() =>
          onChange({ ...filters, view: 'board', sort: filters.sort === 'updated' ? 'manual' : filters.sort })
        }
      >
        <IconBoard />
      </button>
    </div>
  );

  return (
    <div
      className={`${styles.filterBar} ${open ? styles.filterOpen : ''}`}
      role="toolbar"
      aria-label="Filters"
    >
      {!narrow && chipsFold}
      <label className={styles.search}>
        <IconSearch />
        <span className="sr-only">Search issues</span>
        <input type="search" placeholder="Search" value={q} onChange={(e) => setQ(e.target.value)} />
      </label>
      <button
        type="button"
        className={`${ui.btn} ${styles.filterToggle}`}
        aria-expanded={open}
        aria-controls={`${chipsId} ${moreId}`}
        onClick={() => setOpen((o) => !o)}
      >
        Filter{n > 0 ? ` (${n})` : ''}
      </button>
      {narrow ? (
        <>
          {viewToggle}
          {chipsFold}
          {moreFold}
        </>
      ) : (
        <>
          {moreFold}
          {viewToggle}
        </>
      )}
    </div>
  );
}

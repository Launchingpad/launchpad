import { useEffect, useRef, useState } from 'react';
import { useMembers, useProducts, useTeams } from '../../api/queries';
import { useProjectLabels } from '../../api/projectLabels';
import { LabelDot } from '../../ui/LabelPill';
import { Avatar } from '../../ui/Avatar';
import { IconSearch } from '../../ui/Icons';
import { PickerPopover, type PickerOption } from '../../ui/Picker';
import { productTeamsTitle, teamKeysShort } from '../../ui/teamKeys';
import ui from '../../styles/ui.module.css';
import { useProjectStatuses } from '../../api/projectStatuses';
import { ProjectStatusIcon } from './ProjectStatusIcon';
import {
  activeProjectFilters,
  COLUMNS,
  emptyProjectFilters,
  SORT_LABEL,
  type ColumnId,
  type ProjectFilters,
  type SortDir,
  type SortKey,
} from './projectsView';
import styles from './projects.module.css';

const SHORT = 24;
/** A label name in a button is cut (the full name is in the list), so one long name cannot widen the bar. */
const short = (s: string) => ([...s].length > SHORT ? `${[...s].slice(0, SHORT - 1).join('')}…` : s);

function summary(name: string, values: string[]): string {
  if (!values.length) return name;
  return values.length === 1 ? `${name}: ${values[0]}` : `${name}: ${values[0]} +${values.length - 1}`;
}

/** LAUN-165: the filter bar and the Display menu of the projects page (same chips as the issues page). */
export function ProjectFilterBar({
  filters,
  onFilters,
  clearSignal,
  columns,
  onColumns,
  sort,
  dir,
  onSort,
}: {
  filters: ProjectFilters;
  /** Changes when the page clears the filters: the local search text goes too, so the debounce cannot bring it back. */
  clearSignal: number;
  onFilters: (f: ProjectFilters) => void;
  columns: ColumnId[];
  onColumns: (c: ColumnId[]) => void;
  sort: SortKey;
  dir: SortDir;
  onSort: (key: SortKey, dir: SortDir) => void;
}) {
  const members = useMembers();
  const teams = useTeams();
  const products = useProducts();
  const statuses = useProjectStatuses().data ?? [];
  const projectLabels = useProjectLabels();
  const [q, setQ] = useState(filters.q);
  useEffect(() => setQ(filters.q), [filters.q]);
  const seenClear = useRef(clearSignal);
  useEffect(() => {
    if (seenClear.current === clearSignal) return;
    seenClear.current = clearSignal;
    setQ('');
  }, [clearSignal]);
  useEffect(() => {
    if (q === filters.q) return;
    const t = setTimeout(() => onFilters({ ...filters, q }), 250);
    return () => clearTimeout(t);
  }, [q, filters, onFilters]);

  const toggle = <T extends string>(list: T[], v: T): T[] =>
    list.includes(v) ? list.filter((x) => x !== v) : [...list, v];

  const leadOptions: PickerOption[] = [
    { id: 'none', label: 'No lead' },
    ...(members.data ?? []).map((m) => ({
      id: m.username,
      label: m.name,
      hint: m.username,
      icon: <Avatar name={m.name} username={m.username} />,
    })),
  ];
  const leadLabel = filters.lead
    ? (leadOptions.find((o) => o.id === filters.lead)?.label ?? filters.lead)
    : '';
  const teamOptions: PickerOption[] = (teams.data ?? []).map((t) => ({
    id: t.key,
    label: t.name,
    hint: t.key,
  }));
  const productOptions: PickerOption[] = (products.data ?? []).map((p) => ({
    id: p.slug,
    label: p.name,
    hint: teamKeysShort(p.teamKeys),
    title: productTeamsTitle(p.name, p.teamKeys),
  }));
  const productName = filters.product
    ? (products.data?.find((p) => p.slug === filters.product)?.name ?? filters.product)
    : '';
  const showProduct = productOptions.length > 0 || !!filters.product;
  const labelOptions: PickerOption[] = (projectLabels.data ?? []).map((l) => ({
    id: l.id,
    label: l.name,
    hint: projectLabels.data?.find((g) => g.id === l.parentId)?.name,
    icon: <LabelDot color={l.color} />,
  }));
  const labelNames = filters.labels.map(
    (id) =>
      projectLabels.data?.find((l) => l.id === id)?.name ?? (projectLabels.isPending ? '…' : 'Unknown label'),
  );
  // Shown when the org has any project label, or a label is in the address (so it can be cleared).
  const showLabels = labelOptions.length > 0 || filters.labels.length > 0;
  const n = activeProjectFilters(filters);

  return (
    <div className={styles.filterBar} role="toolbar" aria-label="Filters">
      <label className={styles.search}>
        <IconSearch />
        <span className="sr-only">Search projects</span>
        <input type="search" placeholder="Search" value={q} onChange={(e) => setQ(e.target.value)} />
      </label>
      <PickerPopover
        ariaLabel="Filter by status"
        display={summary(
          'Status',
          filters.states.map((id) => statuses.find((s) => s.id === id)?.name ?? ''),
        )}
        options={statuses.map((s) => ({
          id: s.id,
          label: s.name,
          icon: <ProjectStatusIcon kind={s.kind} color={s.color} />,
        }))}
        selected={filters.states}
        multi
        onSelect={(o) => onFilters({ ...filters, states: toggle(filters.states, o.id) })}
      />
      <PickerPopover
        ariaLabel="Filter by lead"
        display={leadLabel ? `Lead: ${leadLabel}` : 'Lead'}
        options={leadOptions}
        selected={filters.lead ? [filters.lead] : []}
        onSelect={(o) => onFilters({ ...filters, lead: filters.lead === o.id ? undefined : o.id })}
      />
      <PickerPopover
        ariaLabel="Filter by team"
        display={summary('Team', filters.teams)}
        options={teamOptions}
        selected={filters.teams}
        multi
        onSelect={(o) => onFilters({ ...filters, teams: toggle(filters.teams, o.id) })}
      />
      {showProduct && (
        <PickerPopover
          ariaLabel="Filter by product"
          display={productName ? `Product: ${productName}` : 'Product'}
          options={productOptions}
          selected={filters.product ? [filters.product] : []}
          onSelect={(o) => onFilters({ ...filters, product: filters.product === o.id ? undefined : o.id })}
        />
      )}
      {showLabels && (
        <PickerPopover
          ariaLabel="Filter by label"
          display={summary('Label', labelNames.map(short))}
          options={labelOptions}
          selected={filters.labels}
          multi
          onSelect={(o) => onFilters({ ...filters, labels: toggle(filters.labels, o.id) })}
        />
      )}
      {n > 0 && (
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost}`}
          onClick={() => {
            setQ('');
            onFilters({ ...emptyProjectFilters });
          }}
        >
          Clear ({n})
        </button>
      )}
      <span className={styles.spacer} />
      <label className={styles.sortPick}>
        <span className="sr-only">Sort by</span>
        <select className={ui.select} value={sort} onChange={(e) => onSort(e.target.value as SortKey, dir)}>
          {(Object.keys(SORT_LABEL) as SortKey[]).map((k) => (
            <option key={k} value={k}>
              {SORT_LABEL[k]}
            </option>
          ))}
        </select>
      </label>
      <button
        type="button"
        className={`${ui.btn} ${ui.ghost}`}
        onClick={() => onSort(sort, dir === 'asc' ? 'desc' : 'asc')}
      >
        {dir === 'asc' ? 'Ascending' : 'Descending'}
      </button>
      <PickerPopover
        ariaLabel="Display columns"
        display="Display"
        align="right"
        multi
        placeholder="Find a column"
        options={COLUMNS.map((c) => ({ id: c.id, label: c.label }))}
        selected={columns}
        onSelect={(o) => {
          // The table draws its columns in COLUMNS order, whatever order they were switched on in.
          onColumns(toggle(columns, o.id as ColumnId));
        }}
      />
    </div>
  );
}

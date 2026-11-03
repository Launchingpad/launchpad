import { useEffect, useState, type ReactNode } from 'react';
import { useIssueSearch } from '../../api/queries';
import type { Issue } from '../../api/types';
import { PickerPopover, type PickerOption } from '../../ui/Picker';
import { StatusIcon } from '../../ui/Icons';

/** Server-side issue search for a picker list: debounced text in, options out. */
export function useIssueSearchOptions({
  exclude = [],
  allowNone,
  noneLabel = 'No parent',
}: {
  exclude?: string[];
  allowNone?: boolean;
  noneLabel?: string;
}) {
  const [text, setText] = useState('');
  const [debounced, setDebounced] = useState('');
  useEffect(() => {
    const t = setTimeout(() => setDebounced(text), 200);
    return () => clearTimeout(t);
  }, [text]);
  const res = useIssueSearch(debounced);
  const found = (res.data ?? []).filter((i) => !exclude.includes(i.identifier));
  const options: PickerOption[] = [
    ...(allowNone && !text ? [{ id: '', label: noneLabel }] : []),
    ...found.map((i) => ({
      id: i.identifier,
      label: `${i.identifier} ${i.title}`,
      icon: <StatusIcon type={i.state.type} color={i.state.color} name={i.state.name} />,
    })),
  ];
  return {
    options,
    onQuery: setText,
    loading: res.isFetching,
    emptyText: text ? 'No matching issues' : 'Type to search',
    /** The issue behind an option id, or null for the "none" row. */
    issueOf: (id: string): Issue | null =>
      id ? ((res.data ?? []).find((i) => i.identifier === id) ?? null) : null,
  };
}

/** Pick another issue by identifier or text (server-side search). */
export function IssueSearchPicker({
  display,
  exclude = [],
  allowNone,
  noneLabel = 'No parent',
  selected,
  onSelect,
  ariaLabel,
  triggerClassName,
  triggerLabel,
}: {
  display: ReactNode;
  exclude?: string[];
  allowNone?: boolean;
  noneLabel?: string;
  selected?: string;
  onSelect: (issue: Issue | null) => void;
  ariaLabel: string;
  triggerClassName?: string;
  triggerLabel?: string;
}) {
  const search = useIssueSearchOptions({ exclude, allowNone, noneLabel });
  return (
    <PickerPopover
      ariaLabel={ariaLabel}
      display={display}
      triggerClassName={triggerClassName}
      triggerLabel={triggerLabel}
      options={search.options}
      selected={selected ? [selected] : []}
      remote
      onQuery={search.onQuery}
      loading={search.loading}
      placeholder="Search by ID or title…"
      emptyText={search.emptyText}
      onSelect={(o) => onSelect(search.issueOf(o.id))}
    />
  );
}

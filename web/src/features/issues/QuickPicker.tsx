import { useAllStates, useLabels, useMembers } from '../../api/queries';
import { LabelDot } from '../../ui/LabelPill';
import type { Issue } from '../../api/types';
import { PRIORITIES } from '../../lib/priority';
import { Avatar } from '../../ui/Avatar';
import { PriorityIcon, StatusIcon } from '../../ui/Icons';
import { PickerDialog, type PickerOption } from '../../ui/Picker';
import type { useIssueActions } from './actions';

export type QuickKind = 'status' | 'assignee' | 'label' | 'priority';

/** The `s`, `a`, `l`, `p` shortcut dialogs. They edit every target issue. */
export function QuickPicker({
  kind,
  targets,
  actions,
  onClose,
}: {
  kind: QuickKind;
  targets: Issue[];
  actions: ReturnType<typeof useIssueActions>;
  onClose: () => void;
}) {
  const { states } = useAllStates();
  const members = useMembers();
  // Targets of one team: that team's labels and counts; a mixed selection gets every label.
  const teamKeys = new Set(targets.map((t) => t.team.key));
  const labels = useLabels(teamKeys.size === 1 ? [...teamKeys][0] : undefined);
  const applyAll = (fn: (i: Issue) => Promise<boolean>) => void Promise.all(targets.map(fn));
  const single = targets.length === 1 ? targets[0]! : undefined;

  if (kind === 'status') {
    const options: PickerOption[] = [];
    for (const s of states) {
      if (targets.length && !targets.some((t) => t.team.id === s.teamId)) continue;
      if (!options.some((o) => o.id === s.name))
        options.push({
          id: s.name,
          label: s.name,
          icon: <StatusIcon type={s.type} color={s.color} name={s.name} />,
        });
    }
    return (
      <PickerDialog
        title="Change status"
        onClose={onClose}
        options={options}
        selected={single ? [single.state.name] : []}
        onSelect={(o) => applyAll((i) => actions.setState(i, o.id))}
      />
    );
  }
  if (kind === 'priority') {
    return (
      <PickerDialog
        title="Set priority"
        onClose={onClose}
        options={PRIORITIES.map((p) => ({
          id: String(p.value),
          label: p.label,
          icon: <PriorityIcon priority={p.value} />,
        }))}
        selected={single ? [String(single.priority)] : []}
        onSelect={(o) => applyAll((i) => actions.setPriority(i, Number(o.id)))}
      />
    );
  }
  if (kind === 'assignee') {
    return (
      <PickerDialog
        title="Assign to"
        onClose={onClose}
        options={[
          { id: '', label: 'Unassigned' },
          ...(members.data ?? []).map((m) => ({
            id: m.username,
            label: m.name,
            hint: m.username,
            icon: <Avatar name={m.name} username={m.username} />,
          })),
        ]}
        selected={single ? [single.assignee?.username ?? ''] : []}
        onSelect={(o) =>
          applyAll((i) => actions.setAssignee(i, members.data?.find((m) => m.username === o.id) ?? null))
        }
      />
    );
  }
  return (
    <PickerDialog
      title="Toggle label"
      multi
      onClose={onClose}
      options={(labels.data ?? []).map((l) => ({
        id: l.id,
        label: l.name,
        icon: <LabelDot color={l.color} />,
      }))}
      selected={(labels.data ?? [])
        .filter((l) => targets.length > 0 && targets.every((t) => t.labels.some((x) => x.id === l.id)))
        .map((l) => l.id)}
      onSelect={(o) => {
        const l = labels.data?.find((x) => x.id === o.id);
        if (!l) return;
        const all = targets.every((t) => t.labels.some((x) => x.id === l.id));
        applyAll((i) => (all ? actions.removeLabel(i, l) : actions.addLabel(i, l)));
      }}
    />
  );
}

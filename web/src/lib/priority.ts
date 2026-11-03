export const PRIORITIES = [
  { value: 1, label: 'Urgent' },
  { value: 2, label: 'High' },
  { value: 3, label: 'Medium' },
  { value: 4, label: 'Low' },
  { value: 0, label: 'No priority' },
] as const;

export function priorityLabel(p: number): string {
  return PRIORITIES.find((x) => x.value === p)?.label ?? 'No priority';
}
/** Sort rank: urgent first, "no priority" last. */
export function priorityRank(p: number): number {
  return p === 0 ? 5 : p;
}

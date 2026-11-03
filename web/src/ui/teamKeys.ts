/**
 * LAUN-100: how a product's teams read next to its name. The short form is for a row or a chip
 * ("ALP, BET, GAM +2"); the long form is for a tooltip and an accessible name (every key, no cut).
 */
export function teamKeysShort(keys: string[], max = 3): string {
  if (keys.length === 0) return 'No teams';
  if (keys.length <= max) return keys.join(', ');
  return `${keys.slice(0, max).join(', ')} +${keys.length - max}`;
}

/** "Alpha: teams ALP, BET" / "Alpha: no teams". */
export function productTeamsTitle(name: string, keys: string[]): string {
  return keys.length === 0
    ? `${name}: no teams`
    : `${name}: ${keys.length === 1 ? 'team' : 'teams'} ${keys.join(', ')}`;
}

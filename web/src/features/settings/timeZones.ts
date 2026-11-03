// A short list for a browser without Intl.supportedValuesOf (older Safari and Firefox). The server accepts any
// IANA name that Go knows, so the list only has to offer the common ones.
const FALLBACK = [
  'Africa/Cairo',
  'Africa/Johannesburg',
  'Africa/Lagos',
  'Africa/Nairobi',
  'America/Anchorage',
  'America/Argentina/Buenos_Aires',
  'America/Bogota',
  'America/Chicago',
  'America/Denver',
  'America/Halifax',
  'America/Los_Angeles',
  'America/Mexico_City',
  'America/New_York',
  'America/Sao_Paulo',
  'America/Toronto',
  'Asia/Bangkok',
  'Asia/Dubai',
  'Asia/Hong_Kong',
  'Asia/Jakarta',
  'Asia/Karachi',
  'Asia/Kolkata',
  'Asia/Seoul',
  'Asia/Shanghai',
  'Asia/Singapore',
  'Asia/Tokyo',
  'Atlantic/Reykjavik',
  'Australia/Melbourne',
  'Australia/Perth',
  'Australia/Sydney',
  'Europe/Amsterdam',
  'Europe/Berlin',
  'Europe/Dublin',
  'Europe/Istanbul',
  'Europe/Lisbon',
  'Europe/London',
  'Europe/Madrid',
  'Europe/Moscow',
  'Europe/Paris',
  'Europe/Rome',
  'Europe/Stockholm',
  'Europe/Zurich',
  'Pacific/Auckland',
  'Pacific/Honolulu',
];

/**
 * The IANA zones to offer: the browser's own list when it has one, else a short fallback. `UTC` is always first
 * (some browsers leave it out) and the zone the org has now is always in the list, so the select never shows a
 * value it cannot hold.
 */
export function timeZoneList(current?: string): string[] {
  let found: string[] = [];
  try {
    const f = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf;
    found = f ? f('timeZone') : [];
  } catch {
    found = [];
  }
  const base = found.length > 0 ? found : FALLBACK;
  return [...new Set(['UTC', ...(current ? [current] : []), ...base])].sort((a, b) =>
    a === 'UTC' ? -1 : b === 'UTC' ? 1 : a.localeCompare(b),
  );
}

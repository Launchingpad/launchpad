// The attachment rules of the server (T3b): the allowlist and the size limit. The server checks
// them again; the web app checks first so a refused file names itself and the reason at once.
export const ATTACH_TYPES = [
  'image/png',
  'image/jpeg',
  'image/gif',
  'video/mp4',
  'text/plain',
  'application/pdf',
];
export const MAX_ATTACH = 100 * 1024 * 1024;

/** Why a file cannot be attached, as a fixed plain phrase, or null when it can. */
export function refusalReason(f: File): string | null {
  if (!ATTACH_TYPES.includes(f.type)) return 'type not allowed (png, jpeg, gif, mp4, text and pdf only)';
  if (f.size < 1) return 'the file is empty';
  if (f.size > MAX_ATTACH) return 'larger than 100 MiB';
  return null;
}

/** Splits a pick or a drop into the files to send and one fixed message for the refused ones. */
export function checkFiles(files: File[]): { ok: File[]; refused: string | null } {
  const ok: File[] = [];
  const bad: string[] = [];
  for (const f of files) {
    const why = refusalReason(f);
    if (why) bad.push(`${f.name} (${why})`);
    else ok.push(f);
  }
  return { ok, refused: bad.length ? `Not attached: ${bad.join('; ')}` : null };
}

export const ATTACH_OFF = 'Attachments are turned off on this server.';

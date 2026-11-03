import { ApiError, errorMessage } from '../../api/errors';
import type { ImportJob, ImportSession } from '../../api/types';

/**
 * The words of the import wizard. Every text here is ours: a failed call never shows the server's text
 * (it could echo something it should not), only a fixed sentence chosen by the status and the code.
 */

export type Step = 1 | 2 | 3 | 4 | 5;

export const STEP_NAMES: Record<Step, string> = {
  1: 'Export from Linear',
  2: 'Match users',
  3: 'Dry run',
  4: 'Import and verify',
  5: 'Turn on cycles',
};

const KIND_STEP: Record<ImportJob['kind'], Step> = { export: 1, dry_run: 3, import: 4, verify: 4 };

function lastJob(s: ImportSession): ImportJob | null {
  const all = [s.jobs.export, s.jobs.dryRun, s.jobs.import, s.jobs.verify].filter((j): j is ImportJob => !!j);
  if (all.length === 0) return null;
  return all.reduce((a, b) => (b.createdAt > a.createdAt ? b : a));
}

/** The step the view opens on (also after a reload), and the furthest step the person may open. */
export function stepsFor(s: ImportSession | null): { current: Step; max: Step } {
  if (!s) return { current: 1, max: 1 };
  const running = [s.jobs.export, s.jobs.dryRun, s.jobs.import, s.jobs.verify].find(
    (j) => j && (j.state === 'queued' || j.state === 'running'),
  );
  const hasExport = Object.keys(s.exportCounts).length > 0;
  let current: Step;
  let max: Step;
  switch (s.state) {
    case 'new':
    case 'exporting':
      current = 1;
      max = 1;
      break;
    case 'exported':
      current = 2;
      max = 2;
      break;
    case 'user_map':
      current = s.hasUserMap ? 3 : 2;
      max = s.hasUserMap ? 3 : 2;
      break;
    case 'dry_run_ok':
      current = 3;
      max = 3;
      break;
    case 'importing':
      current = 4;
      max = 4;
      break;
    case 'imported':
      current = 4;
      max = 5;
      break;
    case 'verified':
      current = 5;
      max = 5;
      break;
    case 'failed': {
      const last = lastJob(s);
      current = last ? KIND_STEP[last.kind] : 1;
      max = s.jobs.import ? 4 : s.jobs.dryRun || s.hasUserMap ? 3 : hasExport ? 2 : 1;
      if (current > max) current = max;
      break;
    }
    default:
      current = 1;
      max = 1;
  }
  if (running) {
    current = KIND_STEP[running.kind];
    if (current > max) max = current;
  }
  return { current, max };
}

/** One sentence for a failed call of the wizard. */
export function importErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return errorMessage(err);
  switch (err.code) {
    case 'linear_unauthorized':
      return 'Linear did not accept that key. Check the key and try again.';
    case 'linear_unreachable':
      return 'Could not reach Linear. Try again in a moment.';
    case 'no_space':
      return 'The server has too little free disk space for the export. Free some space and try again.';
    case 'attachments_unavailable':
      return 'File storage is off on this server, so the import cannot run. An operator must set it up first.';
    case 'public_url_required':
      return 'The server has no public address (PUBLIC_BASE_URL). The import writes it into file links, so an operator must set it first.';
    case 'job_active':
      return 'Another import job is running. Wait until it ends, or cancel it.';
    case 'dry_run_required':
      return 'Run the dry run again first. The export or the user map changed after the last dry run.';
    case 'import_in_progress':
      return 'An import is running. Finish it or close it in Settings, Import from Linear, then try again.';
    case 'rate_limited':
      return 'Too many requests. Wait a moment and try again.';
  }
  if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
  if (err.status === 401) return 'You are signed out. Sign in again.';
  if (err.status === 403) return 'You need admin access to import from Linear.';
  if (err.status === 404) return 'That import was not found. Reload the page.';
  if (err.status === 409)
    return 'The import is not in a state that allows that. Reload the page and look at the current step.';
  if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  if (err.status === 502) return 'Could not reach Linear. Try again in a moment.';
  if (err.status === 507) return 'The server has too little free disk space for the export.';
  return 'Something went wrong. Try again.';
}

/** The words for a user map the server refused (400 validation_failed). */
export function mapErrorText(err: unknown): string {
  if (err instanceof ApiError && err.status === 400)
    return 'The server did not accept the map. Every name must be a member of this organisation. Check each choice.';
  return importErrorText(err);
}

/** What a failed job says. A verify mismatch has its own text (see `mismatchText`). */
export function jobErrorText(job: ImportJob): string {
  switch (job.errorCode) {
    case 'interrupted':
      return 'The job was lost because the server restarted or stopped answering. Start it again.';
    case 'canceled':
      return 'The job was canceled.';
    case 'no_space':
      return 'The server has too little free disk space for the export. Free some space and export again.';
    case 'linear_unauthorized':
      return 'Linear did not accept the key. Export again with a valid key.';
    case 'linear_unreachable':
      return 'The export could not reach Linear. Export again.';
    case 'export_failed':
      return 'The export failed. Try it again. If it fails again, ask an operator to read the server log.';
    case 'dry_run_failed':
      return 'The dry run failed. Check the user map, then run it again. If it fails again, ask an operator to read the server log.';
    case 'import_failed':
      return 'The import failed and kept no rows. Start it again. If it fails again, ask an operator to read the server log.';
    case 'verify_failed':
      return 'The verify failed. Run it again.';
    case 'export_changed':
      return 'The export changed while the job ran. Run the dry run again.';
    default:
      return 'The job failed.';
  }
}

/** decisions-T17 #42c: the two kinds of verify mismatch. */
export function mismatchText(job: ImportJob): string {
  if (job.kind === 'import' && !job.committed)
    return 'The check found a difference before the import was saved, so nothing was written. Check the rows below, then start the import again.';
  return 'The rows were written, but the check found a difference. Run verify again, or run the import again: a second run reports zero creates for rows that match.';
}

export function bytesText(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KiB`;
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MiB`;
  return `${(n / 1024 ** 3).toFixed(2)} GiB`;
}

export function progressText(job: ImportJob): string {
  const unit = job.phase === 'files' ? bytesText : (n: number) => String(n);
  const what = job.phase ? job.phase.replace(/_/g, ' ') : 'starting';
  if (job.total > 0) return `${what}: ${unit(job.done)} of ${unit(job.total)}`;
  return job.state === 'queued' ? 'Waiting to start' : `${what}${job.done > 0 ? `: ${unit(job.done)}` : ''}`;
}

/** Why a step button is off, or null when it is open. The server refuses a user map and a dry run once rows were imported. */
export function stepOffReason(s: ImportSession | null, n: Step, max: Step): string | null {
  if (n > max) return 'Finish the earlier steps first.';
  if (s && (s.state === 'imported' || s.state === 'verified') && (n === 2 || n === 3))
    return 'The server does not allow a new user map or dry run after the import ran. Close this import and export again to change them.';
  return null;
}

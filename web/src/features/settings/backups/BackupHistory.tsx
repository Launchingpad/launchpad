import { useState } from 'react';
import { useBackupChanges, useBackupDestinations, useBackupRuns } from '../../../api/backups';
import type { BackupChange, BackupRun } from '../../../api/types';
import { fullDate } from '../../../lib/time';
import { SkeletonRows } from '../../../ui/States';
import { FixedError } from '../FixedError';
import { SettingsCard, SettingsSection } from '../SettingsParts';
import {
  changeText,
  codeText,
  readSteps,
  RUN_STATUS,
  safeCode,
  sizeText,
  stepLabel,
  TRIGGERS,
} from './backupText';
import styles from './backups.module.css';
import ui from '../../../styles/ui.module.css';

function RunRow({ run }: { run: BackupRun }) {
  const [open, setOpen] = useState(false);
  const steps = readSteps(run);
  const code = safeCode(run.errorCode);
  return (
    <>
      <tr>
        <td>{fullDate(run.queuedAt)}</td>
        <td>{run.destinationName || 'Unsaved form'}</td>
        <td>{TRIGGERS[run.trigger] ?? run.trigger}</td>
        <td>
          {RUN_STATUS[run.status] ?? run.status}
          {run.willRetry && run.status === 'failed' && ' (a retry is planned)'}
        </td>
        <td>{code ? codeText(code) : run.bytes !== null ? sizeText(run.bytes) : '—'}</td>
        <td>
          <button
            type="button"
            className={ui.btn}
            aria-expanded={open}
            aria-label={`Details of the run at ${fullDate(run.queuedAt)}`}
            onClick={() => setOpen(!open)}
          >
            {open ? 'Hide' : 'Details'}
          </button>
        </td>
      </tr>
      {open && (
        <tr>
          <td colSpan={6}>
            <dl className={styles.facts}>
              <dt>Attempt</dt>
              <dd>{run.attempt}</dd>
              <dt>Started</dt>
              <dd>{run.startedAt ? fullDate(run.startedAt) : '—'}</dd>
              <dt>Finished</dt>
              <dd>{run.finishedAt ? fullDate(run.finishedAt) : '—'}</dd>
              <dt>Size</dt>
              <dd>{sizeText(run.bytes)}</dd>
              {code && (
                <>
                  <dt>Code</dt>
                  <dd>
                    <code>{code}</code>
                  </dd>
                </>
              )}
              {steps.length > 0 && (
                <>
                  <dt>Steps</dt>
                  <dd>
                    {steps.map((s, i) => (
                      <div key={`${s.name}-${i}`}>
                        {stepLabel(s.name)}: {s.ok ? 'OK' : `Failed. ${codeText(s.code)}`}
                      </div>
                    ))}
                  </dd>
                </>
              )}
            </dl>
          </td>
        </tr>
      )}
    </>
  );
}

export function BackupRuns() {
  const [dest, setDest] = useState('');
  const destinations = useBackupDestinations();
  // The chosen destination was deleted: show all of them again.
  const gone = dest !== '' && !!destinations.data && !destinations.data.some((d) => d.id === dest);
  const q = useBackupRuns(gone ? '' : dest);
  const runs = q.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <SettingsSection title="Runs" id="backup-runs" description="Backups, tests and other runs, newest first.">
      <div className={ui.field}>
        <label htmlFor="backup-runs-filter">Destination</label>
        <select
          id="backup-runs-filter"
          className={ui.select}
          value={gone ? '' : dest}
          onChange={(e) => setDest(e.target.value)}
        >
          <option value="">All destinations</option>
          {(destinations.data ?? []).map((d) => (
            <option key={d.id} value={d.id}>
              {d.name}
            </option>
          ))}
        </select>
      </div>
      {q.isPending ? (
        <SkeletonRows n={3} />
      ) : q.isError ? (
        <FixedError error={q.error} onRetry={() => void q.refetch()} title="Could not load the runs" />
      ) : (
        <SettingsCard>
          {runs.length === 0 ? (
            <p className={`${ui.hint} ${styles.pad}`}>No run yet.</p>
          ) : (
            <table className={styles.table}>
              <caption className="sr-only">Backup runs</caption>
              <thead>
                <tr>
                  <th scope="col">Queued</th>
                  <th scope="col">Destination</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Status</th>
                  <th scope="col">Result</th>
                  <th scope="col">
                    <span className="sr-only">Details</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {runs.map((r) => (
                  <RunRow key={r.id} run={r} />
                ))}
              </tbody>
            </table>
          )}
        </SettingsCard>
      )}
      {q.hasNextPage && (
        <button
          type="button"
          className={ui.btn}
          disabled={q.isFetchingNextPage}
          onClick={() => void q.fetchNextPage()}
        >
          {q.isFetchingNextPage ? 'Loading…' : 'Show more runs'}
        </button>
      )}
    </SettingsSection>
  );
}

function changeFingerprint(c: BackupChange): string {
  const f = c.detail?.fingerprint;
  return typeof f === 'string' && /^[0-9a-f]{12,64}$/.test(f) ? f.slice(0, 12) : '';
}

export function BackupChanges() {
  const q = useBackupChanges();
  const items = q.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <SettingsSection
      title="Change log"
      id="backup-changes"
      description="Every change to a destination, an encryption key or the notices. Platform admins also get these by mail."
    >
      {q.isPending ? (
        <SkeletonRows n={3} />
      ) : q.isError ? (
        <FixedError error={q.error} onRetry={() => void q.refetch()} title="Could not load the change log" />
      ) : (
        <SettingsCard>
          {items.length === 0 ? (
            <p className={`${ui.hint} ${styles.pad}`}>No change yet.</p>
          ) : (
            <table className={styles.table}>
              <caption className="sr-only">Backup change log</caption>
              <thead>
                <tr>
                  <th scope="col">When</th>
                  <th scope="col">Who</th>
                  <th scope="col">Change</th>
                </tr>
              </thead>
              <tbody>
                {items.map((c) => (
                  <tr key={c.id}>
                    <td>{fullDate(c.at)}</td>
                    <td>{c.actorUsername ?? 'A command on the host'}</td>
                    <td>
                      {changeText(c)}
                      {c.destinationName && <> Destination: {c.destinationName}.</>}
                      {changeFingerprint(c) && (
                        <>
                          {' '}
                          Key <code>{changeFingerprint(c)}</code>.
                        </>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </SettingsCard>
      )}
      {q.hasNextPage && (
        <button
          type="button"
          className={ui.btn}
          disabled={q.isFetchingNextPage}
          onClick={() => void q.fetchNextPage()}
        >
          {q.isFetchingNextPage ? 'Loading…' : 'Show more changes'}
        </button>
      )}
    </SettingsSection>
  );
}

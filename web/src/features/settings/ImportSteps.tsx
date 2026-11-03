import { useState, type FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { activeJob, useImportActions } from '../../api/importer';
import { useOrgSlug } from '../../api/org';
import { useTeams } from '../../api/queries';
import type { ImportJob, ImportSession } from '../../api/types';
import ui from '../../styles/ui.module.css';
import { ErrorState, SkeletonRows } from '../../ui/States';
import { ExportSummary, ErrorLine, FailedFiles, JobFailure, JobProgress, useAct } from './ImportParts';
import styles from './import.module.css';
import settings from './settings.module.css';
import { SettingsCard } from './SettingsParts';
import { TeamCyclesControl } from './TeamCycles';

/* ---------- step 1: the key and the export ---------- */

const PRINTABLE_ASCII = /^[\x21-\x7e]+$/;

/**
 * The Linear key. It lives in this component's state only until the form is sent: the value is copied into
 * a local and the state is cleared at once. When the form unmounts, React drops the state with it. It is never in storage, a URL, the
 * query cache, a log or an error text (every error here is a fixed sentence). The field is a password
 * field with autocomplete off. The request sends no Idempotency-Key.
 */
function KeyForm({ session }: { session: ImportSession | null }) {
  const actions = useImportActions();
  const { busy, error, act } = useAct();
  const [key, setKey] = useState('');
  const [fieldError, setFieldError] = useState<string | null>(null);

  async function submit(e: FormEvent) {
    e.preventDefault();
    const value = key.trim();
    setKey('');
    if (!value) return setFieldError('Paste the Linear API key.');
    if (!PRINTABLE_ASCII.test(value))
      return setFieldError('The key has characters that a Linear key does not have. Paste it again.');
    setFieldError(null);
    await act(async () => {
      const s = session ?? (await actions.start());
      await actions.startExport(s.id, value);
    });
  }

  return (
    <form className={styles.block} onSubmit={(e) => void submit(e)} autoComplete="off" noValidate>
      <div className={ui.field}>
        <label htmlFor="linear-key">Linear API key</label>
        <input
          id="linear-key"
          type="password"
          className={ui.input}
          autoComplete="off"
          spellCheck={false}
          autoCapitalize="off"
          data-1p-ignore
          data-lpignore="true"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          aria-invalid={!!fieldError}
          aria-describedby="linear-key-help"
        />
        {fieldError && (
          <span role="alert" className={settings.fieldError}>
            {fieldError}
          </span>
        )}
        <span id="linear-key-help" className={ui.hint}>
          Make a personal API key in Linear, under Settings, Account, Security. Launchpad uses it once, for
          this export, and does not save it. The field is cleared when you send it. If the export fails, paste
          the key again.
        </span>
      </div>
      <div className={settings.actions}>
        <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={busy}>
          {busy ? 'Starting...' : 'Start the export'}
        </button>
      </div>
      <ErrorLine text={error} />
    </form>
  );
}

export function ExportStep({ session }: { session: ImportSession | null }) {
  const running =
    session?.jobs.export && activeJob(session) === session.jobs.export ? session.jobs.export : null;
  const exported = !!session && Object.keys(session.exportCounts).length > 0;
  const [again, setAgain] = useState(false);
  const failed = session?.jobs.export?.state === 'failed' ? session.jobs.export : null;

  if (running) return <JobProgress job={running} label="Exporting from Linear" />;
  return (
    <>
      <p className={ui.hint}>
        The export copies your Linear data and files to the server. It reads Linear and writes nothing to
        Linear. It can take several minutes.
      </p>
      {failed && <JobFailure job={failed} />}
      {session && <FailedFiles session={session} />}
      {session && exported && <ExportSummary session={session} />}
      {(!session || !exported || again) && <KeyForm session={session} />}
      {session && exported && !again && (
        <div className={settings.actions}>
          <button type="button" className={ui.btn} onClick={() => setAgain(true)}>
            Export again
          </button>
          <span className={ui.hint}>A new export makes the last dry run out of date.</span>
        </div>
      )}
    </>
  );
}

/* ---------- step 3: the dry run ---------- */

function ReportTable({ job }: { job: ImportJob }) {
  const r = job.report;
  if (!r) return null;
  const rows = Object.entries(r.counts).sort(([a], [b]) => a.localeCompare(b));
  return (
    <div className={styles.block} data-testid="dry-run-report">
      <p className={styles.okText}>The dry run finished. Nothing was written.</p>
      <div className={settings.tableScroll}>
        <table className={settings.table}>
          <caption className="sr-only">Dry run counts</caption>
          <thead>
            <tr>
              <th scope="col">Entity</th>
              <th scope="col">In the export</th>
              <th scope="col">Left out</th>
              <th scope="col">Would create</th>
              <th scope="col">Would update</th>
              <th scope="col">Would skip</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(([name, c]) => (
              <tr key={name}>
                <th scope="row">{name.replace(/_/g, ' ')}</th>
                <td>{c.export}</td>
                <td>{c.excluded}</td>
                <td>{c.create}</td>
                <td>{c.update}</td>
                <td>{c.skip}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p data-testid="uploads-planned">
        A real run would upload {r.uploads.planned} {r.uploads.planned === 1 ? 'file' : 'files'}.{' '}
        {r.uploads.present} {r.uploads.present === 1 ? 'is' : 'are'} already in the file store.
      </p>
      {(r.mergedLabels > 0 || r.mergedRelations > 0) && (
        <p className={ui.hint}>
          {r.mergedLabels} labels and {r.mergedRelations} relations would merge into ones that already exist.
        </p>
      )}
      {r.exclusionsTotal > 0 && (
        <details>
          <summary>
            {r.exclusionsTotal} {r.exclusionsTotal === 1 ? 'record is' : 'records are'} left out
          </summary>
          <p className={ui.hint}>
            Showing the first {r.exclusions.length} of {r.exclusionsTotal}.
          </p>
          <div className={settings.tableScroll}>
            <table className={settings.table}>
              <caption className="sr-only">Records left out</caption>
              <thead>
                <tr>
                  <th scope="col">Entity</th>
                  <th scope="col">Linear id</th>
                  <th scope="col">Reason</th>
                </tr>
              </thead>
              <tbody>
                {r.exclusions.map((x, i) => (
                  <tr key={`${x.sourceId}-${i}`}>
                    <td>{x.entity}</td>
                    <td className={styles.mono}>{x.sourceId}</td>
                    <td>{x.reason}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </details>
      )}
    </div>
  );
}

export function DryRunStep({ session }: { session: ImportSession }) {
  const actions = useImportActions();
  const { busy, error, act } = useAct();
  const [accepted, setAccepted] = useState(false);
  const job = session.jobs.dryRun;
  const running = job && activeJob(session) === job ? job : null;
  const stale = session.state === 'user_map' || session.state === 'exported';
  const needsTick = session.failedFiles > 0;

  if (running) return <JobProgress job={running} label="Running the dry run" />;
  return (
    <>
      <p className={ui.hint}>
        A dry run reads the export and the database and shows what a real run would do. It writes no rows and
        uploads no files.
      </p>
      {needsTick && (
        <>
          <FailedFiles session={session} />
          <label className={settings.confirm}>
            <input type="checkbox" checked={accepted} onChange={(e) => setAccepted(e.target.checked)} />
            <span>I read the list of files that failed. Go on without them.</span>
          </label>
        </>
      )}
      <div className={settings.actions}>
        <button
          type="button"
          className={`${ui.btn} ${ui.primary}`}
          disabled={busy || (needsTick && !accepted)}
          onClick={() => void act(() => actions.dryRun(session.id, needsTick && accepted))}
        >
          {job?.state === 'succeeded' ? 'Run the dry run again' : 'Run the dry run'}
        </button>
      </div>
      <ErrorLine text={error} />
      {job && (job.state === 'failed' || job.state === 'canceled') && <JobFailure job={job} />}
      {job?.state === 'succeeded' && (
        <>
          {stale && (
            <p role="status" className={`${ui.banner}`}>
              This report is out of date: the user map or the export changed after it. Run the dry run again.
            </p>
          )}
          <ReportTable job={job} />
        </>
      )}
    </>
  );
}

/* ---------- step 4: import and verify ---------- */

function VerifyRows({ job }: { job: ImportJob }) {
  if (job.rows.length === 0) return null;
  return (
    <div className={styles.block} data-testid="verify-rows">
      <div className={settings.tableScroll}>
        <table className={settings.table}>
          <caption className="sr-only">Verify results</caption>
          <thead>
            <tr>
              <th scope="col">Entity</th>
              <th scope="col">Expected</th>
              <th scope="col">Found</th>
              <th scope="col">Result</th>
            </tr>
          </thead>
          <tbody>
            {job.rows.map((r, i) => {
              const ok = r.expected === r.actual;
              return (
                <tr key={`${r.entity}-${i}`} className={ok ? undefined : styles.bad}>
                  <th scope="row">{r.entity.replace(/_/g, ' ')}</th>
                  <td>{r.expected}</td>
                  <td>{r.actual}</td>
                  <td>
                    {ok ? 'Matches' : 'Differs'}
                    {r.rows && r.rows.length > 0 && (
                      <details>
                        <summary>First {r.rows.length} records</summary>
                        <ul className={styles.names}>
                          {r.rows.map((x, j) => (
                            <li key={j} className={styles.mono}>
                              {x}
                            </li>
                          ))}
                        </ul>
                      </details>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {job.rowsTruncated && <p className={ui.hint}>The list is cut at 500 rows.</p>}
    </div>
  );
}

export function RunStep({ session }: { session: ImportSession }) {
  const org = useOrgSlug();
  const actions = useImportActions();
  const { busy, error, act } = useAct();
  const [typed, setTyped] = useState('');
  const imp = session.jobs.import;
  const ver = session.jobs.verify;
  const running = activeJob(session);
  const dryOk = session.jobs.dryRun?.state === 'succeeded';
  const canStart = ['dry_run_ok', 'failed', 'imported', 'verified'].includes(session.state) && dryOk;
  const hasRows = session.state === 'imported' || session.state === 'verified' || !!imp?.committed;
  const rerun = hasRows;

  if (running && (running.kind === 'import' || running.kind === 'verify'))
    return (
      <>
        <JobProgress job={running} label={running.kind === 'import' ? 'Importing' : 'Verifying'} />
        {running.kind === 'import' && (
          <p className={ui.hint}>
            The import is one transaction. If it stops, nothing is saved, and you start it again.
          </p>
        )}
      </>
    );

  const typedOk = typed.trim() === org;
  return (
    <>
      <div className={`${ui.banner}`} role="note">
        <div>
          <p>
            A stopped import keeps no rows. If you cancel it or the server restarts, you start it again. Files
            that were already uploaded stay, so the second run is faster.
          </p>
          <p>
            {rerun ? 'You ran this import before. ' : ''}A run sets the imported rows to what Linear has.
            Edits you made to imported rows in Launchpad are lost when you run the import again.
          </p>
        </div>
      </div>

      {imp?.state === 'failed' && (
        <>
          <JobFailure job={imp} />
          {imp.errorCode === 'verify_mismatch' && <VerifyRows job={imp} />}
        </>
      )}
      {imp?.state === 'canceled' && <JobFailure job={imp} />}
      {imp?.state === 'succeeded' && (
        <p role="status" className={styles.okText}>
          The import finished and its rows are saved. Run verify to check them.
        </p>
      )}

      <form
        className={styles.block}
        onSubmit={(e) => {
          e.preventDefault();
          if (typedOk) void act(() => actions.run(session.id, typed.trim()));
        }}
      >
        <div className={ui.field}>
          <label htmlFor="confirm-org">
            Type the organisation slug <code>{org}</code> to confirm
          </label>
          <input
            id="confirm-org"
            className={ui.input}
            autoComplete="off"
            spellCheck={false}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
          />
        </div>
        <div className={settings.actions}>
          <button type="submit" className={`${ui.btn} ${ui.danger}`} disabled={busy || !typedOk || !canStart}>
            {rerun ? 'Run the import again' : 'Start the import'}
          </button>
          {!dryOk && <span className={ui.hint}>Run a dry run first (step 3).</span>}
        </div>
        <ErrorLine text={error} />
      </form>

      {(session.state === 'imported' || session.state === 'verified' || imp?.committed) && (
        <div className={styles.block}>
          <div className={settings.actions}>
            <button
              type="button"
              className={ui.btn}
              disabled={busy}
              onClick={() => void act(() => actions.verify(session.id))}
            >
              {ver ? 'Run verify again' : 'Run verify'}
            </button>
            <span className={ui.hint}>Verify compares the export with the database. It writes nothing.</span>
          </div>
          {ver?.state === 'failed' && <JobFailure job={ver} />}
          {ver?.state === 'canceled' && <JobFailure job={ver} />}
          {ver?.state === 'succeeded' && (
            <p role="status" className={styles.okText}>
              Verify passed: every count matches.
            </p>
          )}
          {ver && <VerifyRows job={ver} />}
        </div>
      )}
    </>
  );
}

/* ---------- step 5: cycles ---------- */

export function CyclesStep({ session }: { session: ImportSession }) {
  const teams = useTeams();
  const [error, setError] = useState<string | null>(null);
  const blocked = ['exporting', 'exported', 'user_map', 'dry_run_ok', 'importing'].includes(session.state);
  return (
    <>
      <p>
        Cycles are off by default. Turn them on after the import, so the cycle job continues from the highest
        cycle number that came from Linear. If you turn cycles on before the import, the job makes a cycle 1
        that clashes with the Linear numbers.
      </p>
      <p className={ui.hint}>
        The server refuses to turn cycles on while an import session is in the export, user map, dry run or
        import step. It answers {'"'}An import is running{'"'}. Finish the import, or close the session, and
        try again. Turning cycles off is never blocked.
      </p>
      {blocked && (
        <p role="status" className={ui.banner}>
          An import is running, so the switches fail for now.
        </p>
      )}
      {teams.isPending ? (
        <SkeletonRows n={2} />
      ) : teams.isError ? (
        <ErrorState error={teams.error} onRetry={() => void teams.refetch()} title="Could not load teams" />
      ) : (
        <SettingsCard>
          <table className={`${settings.table} ${settings.fitTable}`}>
            <caption className="sr-only">Cycles per team</caption>
            <thead>
              <tr>
                <th scope="col">Team</th>
                <th scope="col">Cycles</th>
              </tr>
            </thead>
            <tbody>
              {teams.data.map((t) => (
                <tr key={t.id}>
                  <td>
                    <code>{t.key}</code> {t.name}
                  </td>
                  <td>
                    <TeamCyclesControl team={t} canEdit onError={setError} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </SettingsCard>
      )}
      <ErrorLine text={error} />
      <p className={ui.hint}>
        You can also switch cycles on in <Link to="../teams">Settings, Teams</Link>.
      </p>
    </>
  );
}

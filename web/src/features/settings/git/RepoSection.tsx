import { useEffect, useRef, useState, type FormEvent } from 'react';
import { useAvailableRepos, useGitActions } from '../../../api/gitIntegrations';
import { useTeams } from '../../../api/queries';
import type { GitIntegration, GitRepo, Team } from '../../../api/types';
import { Dialog } from '../../../ui/Dialog';
import { SettingsCard, SettingsSection } from '../SettingsParts';
import { backfillText, gitErrorText, hookText, refreshText } from './gitText';
import { touchClass } from './gitParts';
import styles from './git.module.css';
import settings from '../settings.module.css';
import ui from '../../../styles/ui.module.css';

function TeamSelect({
  id,
  label,
  value,
  teams,
  disabled,
  onChange,
}: {
  id?: string;
  label: string;
  value: string;
  teams: Team[];
  disabled?: boolean;
  onChange: (teamId: string) => void;
}) {
  return (
    <select
      id={id}
      className={ui.select}
      aria-label={id ? undefined : label}
      value={value}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value)}
    >
      <option value="">Any team</option>
      {teams.map((t) => (
        <option key={t.id} value={t.id}>
          {t.name} ({t.key})
        </option>
      ))}
    </select>
  );
}

/** The mapped repositories, and the way to map more (decisions-T17 #47). */
export function RepoSection({ integration, repos }: { integration: GitIntegration; repos: GitRepo[] }) {
  const actions = useGitActions(integration.id);
  const teams = useTeams().data ?? [];
  const [target, setTarget] = useState<GitRepo | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const tokenMode = integration.mode === 'token';
  const alertRef = useRef<HTMLParagraphElement>(null);
  // The picker list can be far from the table: bring a new error into view (jsdom has no scrollIntoView).
  useEffect(() => {
    if (error) alertRef.current?.scrollIntoView?.({ block: 'nearest', behavior: 'smooth' });
  }, [error]);
  // One host call at a time (the calls are rate limited like Check token).
  const [busy, setBusy] = useState<{ repo: string; what: 'hook' | 'backfill' } | null>(null);

  async function refreshHook(repo: GitRepo) {
    if (busy) return;
    setError(null);
    setNote(null);
    setBusy({ repo: repo.id, what: 'hook' });
    try {
      const r = await actions.refreshHook.mutateAsync(repo.id);
      setNote(`${repo.repoPath}: ${refreshText(r.hookState)}`);
    } catch (err) {
      setError(`${repo.repoPath}: ${gitErrorText(err, 'hook', integration.provider)}`);
    } finally {
      setBusy(null);
    }
  }

  async function backfill(repo: GitRepo) {
    if (busy) return;
    setError(null);
    setNote(null);
    setBusy({ repo: repo.id, what: 'backfill' });
    try {
      const r = await actions.backfill.mutateAsync(repo.id);
      setNote(`${repo.repoPath}: ${backfillText(r)}`);
    } catch (err) {
      setError(`${repo.repoPath}: ${gitErrorText(err, 'backfill', integration.provider)}`);
    } finally {
      setBusy(null);
    }
  }

  async function unmap() {
    if (!target) return;
    setError(null);
    setNote(null);
    const t = target;
    try {
      const r = await actions.unmap.mutateAsync(t.id);
      setNote(
        r.hookRemoved
          ? `${t.repoPath} is unmapped. The hook is removed from the host.`
          : `${t.repoPath} is unmapped, but the hook may still be on the host. Remove it there by hand.`,
      );
    } catch (err) {
      setError(gitErrorText(err, 'repos', integration.provider));
    } finally {
      setTarget(null);
    }
  }

  async function changeTeam(repo: GitRepo, teamId: string) {
    setError(null);
    setNote(null);
    try {
      await actions.setTeam.mutateAsync({ repo: repo.id, teamId: teamId || null });
    } catch (err) {
      setError(gitErrorText(err, 'repos', integration.provider));
    }
  }

  return (
    <SettingsSection
      title="Repositories"
      description="A merge request or pull request in a mapped repository moves the ticket it names: by default to the team's review state when it opens and to its testing state when it merges. Each team's Git workflow page can change these moves. Set a team to link only that team's tickets."
    >
      {repos.length === 0 ? (
        <p className={ui.hint}>No repository is mapped yet.</p>
      ) : (
        <SettingsCard>
          <div className={settings.tableScroll}>
            <table className={settings.table}>
              <caption className="sr-only">Mapped repositories</caption>
              <thead>
                <tr>
                  <th scope="col">Repository</th>
                  <th scope="col">Team</th>
                  <th scope="col">Hook</th>
                  <th scope="col">
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {repos.map((r) => (
                  <tr key={r.id}>
                    <td className={styles.cell}>
                      {r.repoPath}
                      <div className={styles.meta}>id {r.repoKey}</div>
                    </td>
                    <td>
                      <TeamSelect
                        label={`Team for ${r.repoPath}`}
                        value={r.team?.id ?? ''}
                        teams={teams}
                        disabled={actions.setTeam.isPending}
                        onChange={(v) => void changeTeam(r, v)}
                      />
                    </td>
                    <td className={styles.cell}>
                      <span
                        className={`${styles.chip} ${r.hookState === 'failed' ? styles.chipWarn : r.hookState === 'none' ? styles.chipOff : ''}`}
                      >
                        {hookText(r)}
                      </span>
                    </td>
                    <td>
                      <div className={styles.slot} data-slot="repo-actions">
                        {tokenMode && (
                          <>
                            {/* aria-disabled, not disabled: a disabled button drops keyboard focus while the call runs. */}
                            <button
                              type="button"
                              className={ui.btn}
                              aria-label={`Refresh hooks for ${r.repoPath}`}
                              aria-disabled={busy !== null || undefined}
                              aria-busy={(busy?.repo === r.id && busy.what === 'hook') || undefined}
                              onClick={() => void refreshHook(r)}
                            >
                              {busy?.repo === r.id && busy.what === 'hook' ? 'Refreshing…' : 'Refresh hooks'}
                            </button>
                            <button
                              type="button"
                              className={ui.btn}
                              aria-label={`Backfill open MRs for ${r.repoPath}`}
                              aria-disabled={busy !== null || undefined}
                              aria-busy={(busy?.repo === r.id && busy.what === 'backfill') || undefined}
                              onClick={() => void backfill(r)}
                            >
                              {busy?.repo === r.id && busy.what === 'backfill'
                                ? 'Reading…'
                                : 'Backfill open MRs'}
                            </button>
                          </>
                        )}
                        <button
                          type="button"
                          className={`${ui.btn} ${ui.danger}`}
                          aria-label={`Unmap ${r.repoPath}`}
                          onClick={() => {
                            setNote(null);
                            setError(null);
                            setTarget(r);
                          }}
                        >
                          Unmap
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </SettingsCard>
      )}
      {/* Always mounted, so a screen reader hears the text when it appears. */}
      <p role="status" className={note ? ui.hint : 'sr-only'}>
        {note}
      </p>
      {error && (
        <p ref={alertRef} role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}

      <h3 className={settings.subtitle}>Map a repository</h3>
      <SettingsCard padded>
        {tokenMode ? (
          <RepoPicker
            integration={integration}
            teams={teams}
            mappedKeys={new Set(repos.map((r) => r.repoKey))}
            onError={setError}
            onNote={setNote}
          />
        ) : (
          <WebhookRepoForm integration={integration} teams={teams} onError={setError} onNote={setNote} />
        )}
      </SettingsCard>

      {target && (
        <Dialog
          className={touchClass}
          title="Unmap this repository?"
          onClose={() => setTarget(null)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setTarget(null)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                disabled={actions.unmap.isPending}
                onClick={() => void unmap()}
              >
                {actions.unmap.isPending ? 'Unmapping…' : 'Unmap'}
              </button>
            </>
          }
        >
          <p>
            <strong>{target.repoPath}</strong> stops moving tickets.{' '}
            {tokenMode
              ? 'Launchpad removes the hook from the host. If it cannot, it tells you.'
              : 'Remove the hook on the host yourself.'}{' '}
            Links already made stay.
          </p>
        </Dialog>
      )}
    </SettingsSection>
  );
}

/** Token mode: search the provider's repositories (a page at a time) and map one. */
function RepoPicker({
  integration,
  teams,
  mappedKeys,
  onError,
  onNote,
}: {
  integration: GitIntegration;
  teams: Team[];
  /** The host ids of the mapped repos, from the live repo list: a map or an unmap shows here at once. */
  mappedKeys: ReadonlySet<string>;
  onError: (t: string | null) => void;
  onNote: (t: string | null) => void;
}) {
  const actions = useGitActions(integration.id);
  const [input, setInput] = useState('');
  const [q, setQ] = useState('');
  const [started, setStarted] = useState(false);
  const [teamId, setTeamId] = useState('');
  const [busyRepo, setBusyRepo] = useState<string | null>(null);
  const found = useAvailableRepos(integration.id, q, started);
  const loaded = found.data?.pages.flatMap((p) => p.items) ?? [];
  // A mapped repo is not offered again (LAUN-113). The filter runs on the loaded pages, so Load more and the
  // cursor are untouched; a mapped one comes back when the live repo list drops it.
  const rows = loaded.filter((r) => !mappedKeys.has(r.repoKey));

  function search(e: FormEvent) {
    e.preventDefault();
    onError(null);
    onNote(null);
    const next = input.trim();
    if (started && next === q) {
      // Same search: nothing refetches by itself, so a press is the retry after a failure. After a failed
      // Load more the pages already loaded stay, and only the next page is asked for again.
      if (found.isError) void (found.data ? found.fetchNextPage() : found.refetch());
      return;
    }
    setQ(next);
    setStarted(true);
  }

  async function map(repoId: string, path: string) {
    onError(null);
    onNote(null);
    setBusyRepo(repoId);
    try {
      await actions.mapRepo.mutateAsync({ repoId, ...(teamId ? { teamId } : {}) });
      onNote(`${path} is mapped. Launchpad made the hook.`);
    } catch (err) {
      onError(gitErrorText(err, 'repos', integration.provider));
    } finally {
      setBusyRepo(null);
    }
  }

  return (
    <>
      <form onSubmit={search} className={styles.search} role="search" aria-label="Find repositories">
        <label htmlFor="git-repo-q" className="sr-only">
          Search repositories
        </label>
        <input
          id="git-repo-q"
          className={ui.input}
          type="search"
          placeholder="Search repositories by name"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          autoComplete="off"
        />
        <button type="submit" className={ui.btn} disabled={found.isFetching}>
          {started ? 'Search' : 'Find repositories'}
        </button>
      </form>
      <div className={ui.field}>
        <label htmlFor="git-map-team">Team for the new mapping</label>
        <TeamSelect id="git-map-team" label="Team" value={teamId} teams={teams} onChange={setTeamId} />
      </div>
      {started && found.isPending && !found.isError && (
        <p role="status" className={ui.hint}>
          Asking {integration.name}…
        </p>
      )}
      {started && found.isError && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {gitErrorText(found.error, 'list', integration.provider)}{' '}
          {found.data ? 'Press Load more to try again.' : 'Press Search to try again.'}
        </p>
      )}
      {started && found.data && (
        <>
          <ul className={styles.list} aria-label="Repositories on the host">
            {rows.map((r) => (
              <li key={r.repoId} className={styles.pickRow}>
                <span className={styles.cell}>{r.repoPath}</span>
                <button
                  type="button"
                  className={ui.btn}
                  aria-label={`Map ${r.repoPath}`}
                  disabled={busyRepo !== null}
                  onClick={() => void map(r.repoId, r.repoPath)}
                >
                  {busyRepo === r.repoId ? 'Mapping…' : 'Map'}
                </button>
              </li>
            ))}
          </ul>
          {rows.length === 0 && (
            <p role="status" className={ui.hint}>
              {loaded.length > 0
                ? found.hasNextPage
                  ? 'Every repository loaded so far is already mapped. Load more.'
                  : 'Every repository found is already mapped.'
                : found.hasNextPage
                  ? 'No repository on this page matches. Load more.'
                  : 'No repository found. Check the search, and that the token can see the repository.'}
            </p>
          )}
          {found.hasNextPage && (
            <div className={settings.actions}>
              <button
                type="button"
                className={ui.btn}
                disabled={found.isFetchingNextPage}
                onClick={() => void found.fetchNextPage()}
              >
                {found.isFetchingNextPage ? 'Loading…' : 'Load more'}
              </button>
            </div>
          )}
        </>
      )}
      {!started && (
        <p className={ui.hint}>
          Press Find repositories to list what the token can see. Launchpad makes the hook when you map one.
        </p>
      )}
    </>
  );
}

const KEY_RULE = 'Enter the numeric repository id: 1 to 18 digits.';
const BITBUCKET_KEY = /^([0-9]{1,18}|\{[0-9a-fA-F-]{36}\}\/\{[0-9a-fA-F-]{36}\})$/;
const BITBUCKET_KEY_RULE =
  'Enter the numeric repository id (Data Center), or {workspace uuid}/{repository uuid} with the braces (Cloud).';

/** Webhook only: the admin names the repository (id and path) and makes the hook on the host. */
function WebhookRepoForm({
  integration,
  teams,
  onError,
  onNote,
}: {
  integration: GitIntegration;
  teams: Team[];
  onError: (t: string | null) => void;
  onNote: (t: string | null) => void;
}) {
  const actions = useGitActions(integration.id);
  const [key, setKey] = useState('');
  const [path, setPath] = useState('');
  const [teamId, setTeamId] = useState('');
  const [errors, setErrors] = useState<{ key?: string; path?: string }>({});
  const bitbucket = integration.provider === 'bitbucket';
  const hint = bitbucket
    ? 'Bitbucket Data Center: the repository id in the hook payload (repository.id), with the path PROJECT/slug. Bitbucket Cloud: repository.workspace.uuid, a slash, then repository.uuid, both with braces.'
    : integration.provider === 'github'
      ? 'GitHub: the numeric id from GET /repos/{owner}/{repo}.'
      : 'GitLab: Settings, General, Project ID.';

  async function submit(e: FormEvent) {
    e.preventDefault();
    onError(null);
    onNote(null);
    const k = key.trim();
    const p = path.trim();
    const next: { key?: string; path?: string } = {};
    if (!(bitbucket ? BITBUCKET_KEY : /^[0-9]{1,18}$/).test(k))
      next.key = bitbucket ? BITBUCKET_KEY_RULE : KEY_RULE;
    if (p === '' || p.length > 300) next.path = 'Enter the repository path, for example group/project.';
    setErrors(next);
    if (next.key || next.path) return;
    try {
      await actions.mapRepo.mutateAsync({ repoKey: k, repoPath: p, ...(teamId ? { teamId } : {}) });
      setKey('');
      setPath('');
      onNote(`${p} is mapped. Add the hook on the host if you have not.`);
    } catch (err) {
      onError(gitErrorText(err, 'repos', integration.provider));
    }
  }

  return (
    <form onSubmit={(e) => void submit(e)} noValidate aria-label="Map a repository by id">
      <p className={ui.hint}>
        Do not want to type ids? A connection with an access token can pick repositories from a list and make
        the hooks itself. Use Add a token at the top of this page.
      </p>
      <div className={ui.field}>
        <label htmlFor="git-repo-key">Repository id</label>
        <input
          id="git-repo-key"
          className={ui.input}
          inputMode={bitbucket ? 'text' : 'numeric'}
          value={key}
          onChange={(e) => setKey(e.target.value)}
          autoComplete="off"
          aria-invalid={!!errors.key}
          aria-describedby={errors.key ? 'git-repo-key-error' : 'git-repo-key-hint'}
        />
        {errors.key && (
          <span id="git-repo-key-error" className={settings.fieldError} aria-live="polite">
            {errors.key}
          </span>
        )}
        <span id="git-repo-key-hint" className={ui.hint} hidden={!!errors.key}>
          {bitbucket ? 'The key the host sends in its hook.' : 'The number the host sends in its hook.'}{' '}
          {hint}
        </span>
      </div>
      <div className={ui.field}>
        <label htmlFor="git-repo-path">Repository path</label>
        <input
          id="git-repo-path"
          className={ui.input}
          value={path}
          onChange={(e) => setPath(e.target.value)}
          autoComplete="off"
          placeholder="group/project"
          aria-invalid={!!errors.path}
          aria-describedby={errors.path ? 'git-repo-path-error' : undefined}
        />
        {errors.path && (
          <span id="git-repo-path-error" className={settings.fieldError} aria-live="polite">
            {errors.path}
          </span>
        )}
      </div>
      <div className={ui.field}>
        <label htmlFor="git-map-team">Team</label>
        <TeamSelect id="git-map-team" label="Team" value={teamId} teams={teams} onChange={setTeamId} />
      </div>
      <div className={settings.actions}>
        <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={actions.mapRepo.isPending}>
          {actions.mapRepo.isPending ? 'Mapping…' : 'Map repository'}
        </button>
      </div>
    </form>
  );
}

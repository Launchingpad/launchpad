import { useState, type FormEvent } from 'react';
import { useImportActions, useImportUsers } from '../../api/importer';
import { ApiError } from '../../api/errors';
import { useCreateAgent } from '../../api/queries';
import type { ImportSession, ImportUsers } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { ErrorState, SkeletonRows } from '../../ui/States';
import ui from '../../styles/ui.module.css';
import { ErrorLine, useAct } from './ImportParts';
import styles from './import.module.css';
import settings from './settings.module.css';
import { mapErrorText } from './importText';

const USERNAME_RE = /^[a-z][a-z0-9_-]{0,38}$/;
const USERNAME_RULE = 'Use 1 to 39 characters: a-z, 0-9, _ and -. Start with a letter.';

/** Step 2. The map has only existing members: "Create agent" makes the agent at once, then picks it (T17 #45). */
export function UserMapStep({ session, onSaved }: { session: ImportSession; onSaved: () => void }) {
  const users = useImportUsers(session.id);
  if (users.isPending) return <SkeletonRows n={4} />;
  if (users.isError)
    return (
      <ErrorState
        error={users.error}
        onRetry={() => void users.refetch()}
        title="Could not load the Linear users"
      />
    );
  return <MapEditor session={session} data={users.data} onSaved={onSaved} />;
}

function MapEditor({
  session,
  data,
  onSaved,
}: {
  session: ImportSession;
  data: ImportUsers;
  onSaved: () => void;
}) {
  const actions = useImportActions();
  const { busy, error, act, clear } = useAct();
  const [validation, setValidation] = useState<string | null>(null);
  const [def, setDef] = useState(data.map?.default ?? '');
  // The saved choice wins, then the server's suggestion.
  const [chosen, setChosen] = useState<Record<string, string>>(() =>
    Object.fromEntries(data.users.map((u) => [u.id, u.mapped ?? (data.map ? '' : (u.suggested ?? ''))])),
  );
  const [members, setMembers] = useState(data.members);
  const [creatingFor, setCreatingFor] = useState<string | null>(null);

  const options = members.map((m) => (
    <option key={m.username} value={m.username}>
      {m.name} ({m.username}){m.isAgent ? ', agent' : ''}
    </option>
  ));

  const unmapped = data.users.filter((u) => !u.app && !chosen[u.id]);

  async function save(e: FormEvent) {
    e.preventDefault();
    clear();
    if (!def && unmapped.length > 0) {
      setValidation(
        `${unmapped.length} ${unmapped.length === 1 ? 'person has' : 'people have'} no member. Choose a member for each person, or choose a default member for everyone else.`,
      );
      return;
    }
    setValidation(null);
    const map = {
      default: def,
      users: Object.fromEntries(Object.entries(chosen).filter(([, v]) => v)),
    };
    const ok = await act(() => actions.saveMap(session.id, map), mapErrorText);
    if (ok) onSaved();
  }

  return (
    <form onSubmit={(e) => void save(e)} className={styles.block}>
      <p className={ui.hint}>
        Each person in Linear needs a member of this organisation. Pick a member, or make an agent. People who
        are not matched get the default member. A human member needs an invite first (Settings, Members).
      </p>
      <div className={ui.field}>
        <label htmlFor="map-default">Default member for everyone else</label>
        <select
          id="map-default"
          className={`${ui.select} ${styles.mapSelect}`}
          value={def}
          onChange={(e) => setDef(e.target.value)}
        >
          <option value="">No default</option>
          {options}
        </select>
      </div>
      <div className={settings.tableScroll}>
        <table className={settings.table}>
          <caption className="sr-only">Linear users and their members</caption>
          <thead>
            <tr>
              <th scope="col">Linear user</th>
              <th scope="col">Member</th>
              <th scope="col">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {data.users.map((u) => (
              <tr key={u.id}>
                <td>
                  <span className={styles.cell}>
                    <span>
                      {u.displayName || u.name}
                      {!u.active && <span className={settings.tag}>inactive</span>}
                      {u.app && <span className={settings.tag}>app</span>}
                    </span>
                    <span className={ui.hint}>{u.email}</span>
                  </span>
                </td>
                <td>
                  <select
                    className={`${ui.select} ${styles.mapSelect}`}
                    aria-label={`Member for ${u.displayName || u.name}`}
                    value={chosen[u.id] ?? ''}
                    onChange={(e) => setChosen((c) => ({ ...c, [u.id]: e.target.value }))}
                  >
                    <option value="">{def ? `Default (${def})` : 'Not chosen'}</option>
                    {options}
                  </select>
                </td>
                <td>
                  <button type="button" className={ui.btn} onClick={() => setCreatingFor(u.id)}>
                    Create agent
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {validation && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {validation}
        </p>
      )}
      <ErrorLine text={error} />
      <div className={settings.actions}>
        <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={busy}>
          Save the map and continue
        </button>
        <span className={ui.hint}>Saving makes an earlier dry run out of date.</span>
      </div>
      {creatingFor && (
        <CreateAgentDialog
          onClose={() => setCreatingFor(null)}
          onCreated={(m) => {
            setMembers((all) => (all.some((x) => x.username === m.username) ? all : [...all, m]));
            setChosen((c) => ({ ...c, [creatingFor]: m.username }));
            setCreatingFor(null);
          }}
          suggest={data.users.find((u) => u.id === creatingFor)?.displayName ?? ''}
        />
      )}
    </form>
  );
}

function CreateAgentDialog({
  onClose,
  onCreated,
  suggest,
}: {
  onClose: () => void;
  onCreated: (m: { username: string; name: string; isAgent: boolean }) => void;
  suggest: string;
}) {
  const create = useCreateAgent();
  const [username, setUsername] = useState('');
  const [name, setName] = useState(suggest);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    e.stopPropagation();
    const u = username.trim().toLowerCase();
    const n = name.trim();
    if (!USERNAME_RE.test(u)) return setError(USERNAME_RULE);
    if (n.length < 1 || [...n].length > 200) return setError('Enter 1 to 200 characters for the name.');
    setBusy(true);
    setError(null);
    try {
      const m = await create.mutateAsync({ username: u, name: n });
      onCreated({ username: m.username, name: m.name, isAgent: true });
    } catch (err) {
      if (err instanceof ApiError && err.status === 409)
        setError('This username is in use. Use another username.');
      else if (err instanceof ApiError && err.status === 403)
        setError('You need admin access to create an agent.');
      else setError('Could not create the agent. Try again.');
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog title="Create an agent" width={460} onClose={onClose}>
      <form className={styles.dialogForm} onSubmit={(e) => void submit(e)}>
        <p className={ui.hint}>
          An agent has no email and cannot sign in. The map then uses it for this person.
        </p>
        <div className={ui.field}>
          <label htmlFor="ia-username">Username</label>
          <input
            id="ia-username"
            className={ui.input}
            data-autofocus
            autoComplete="off"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
        </div>
        <div className={ui.field}>
          <label htmlFor="ia-name">Name</label>
          <input
            id="ia-name"
            className={ui.input}
            autoComplete="off"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <ErrorLine text={error} />
        <div className={settings.actions}>
          <button type="button" className={`${ui.btn} ${ui.ghost}`} onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={busy}>
            Create agent
          </button>
        </div>
      </form>
    </Dialog>
  );
}

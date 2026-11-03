import { useState } from 'react';
import { useMembers } from '../../api/queries';
import type { Member } from '../../api/types';
import ui from '../../styles/ui.module.css';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { useIsOrgAdmin } from './access';
import { InvitesSection } from './InvitesSection';
import styles from './settings.module.css';
import { SettingsCard, SettingsPage } from './SettingsParts';
import { TeamAccessDialog } from './TeamAccessDialog';
import { accessLabel } from './teamAccessText';

/**
 * The member list. The API has no route to change a role or remove a person. An org admin sees each member's team
 * access and edits it (decisions-T20 #74, LAUN-227); an admin row shows "All teams" and no action (#2), because the
 * server refuses an admin target. The server sends `teamAccess` to an org admin only. Invites are on the same page.
 */
export function MembersPage() {
  const members = useMembers();
  const isAdmin = useIsOrgAdmin();
  const [editing, setEditing] = useState<Member | null>(null);
  const [note, setNote] = useState<string | null>(null);
  // The column needs the server's fields: they are null unless the caller is an org admin.
  const showAccess = isAdmin && !!members.data?.some((m) => m.teamAccess !== null);

  return (
    <SettingsPage title="Members" wide>
      {members.isPending ? (
        <SkeletonRows n={4} />
      ) : members.isError ? (
        <ErrorState
          error={members.error}
          onRetry={() => void members.refetch()}
          title="Could not load members"
        />
      ) : members.data.length === 0 ? (
        <EmptyState title="No members" />
      ) : (
        <SettingsCard>
          {/* SettingsCard scrolls a wide table sideways on a narrow screen (min-width 680px). */}
          <table className={styles.table}>
            <caption className="sr-only">Members</caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Username</th>
                <th scope="col">Email</th>
                <th scope="col">Role</th>
                {showAccess && (
                  <>
                    <th scope="col">Teams</th>
                    <th scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  </>
                )}
              </tr>
            </thead>
            <tbody>
              {members.data.map((m) => (
                <tr key={m.id}>
                  <td>
                    {m.name}
                    {m.isAgent && <span className={styles.tag}>Agent</span>}
                  </td>
                  <td>
                    <code>{m.username}</code>
                  </td>
                  <td>{m.email ?? '—'}</td>
                  <td>{m.role === 'admin' ? 'Admin' : 'Member'}</td>
                  {showAccess && (
                    <>
                      <td data-testid={`access-${m.username}`}>{accessLabel(m)}</td>
                      <td>
                        {m.role !== 'admin' && m.teamAccess !== null && (
                          <button
                            type="button"
                            className={ui.btn}
                            aria-label={`Edit team access for ${m.name}`}
                            onClick={() => {
                              setNote(null);
                              setEditing(m);
                            }}
                          >
                            Edit access
                          </button>
                        )}
                      </td>
                    </>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </SettingsCard>
      )}
      <p role="status" className={ui.hint} data-testid="access-note">
        {note}
      </p>
      {editing && (
        <TeamAccessDialog
          key={editing.id}
          member={editing}
          onClose={() => setEditing(null)}
          onSaved={(text) => {
            setNote(text);
            setEditing(null);
          }}
        />
      )}
      <InvitesSection />
    </SettingsPage>
  );
}

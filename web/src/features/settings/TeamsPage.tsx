import { useState } from 'react';
import { useTeams } from '../../api/queries';
import ui from '../../styles/ui.module.css';
import { useIsOrgAdmin } from './access';
import { TeamCyclesControl } from './TeamCycles';
import { EmptyState, ErrorState, SkeletonRows } from '../../ui/States';
import { TeamForm } from './TeamForm';
import { RenameTeamDialog } from './TeamRename';
import { DeleteTeamDialog } from './TeamDelete';
import { touchClass } from './OrganisationPage';
import type { Team } from '../../api/types';
import styles from './settings.module.css';
import { SettingsCard, SettingsPage } from './SettingsParts';

export function TeamsPage() {
  const teams = useTeams();
  const isAdmin = useIsOrgAdmin();
  const [cyclesError, setCyclesError] = useState<string | null>(null);
  const [renaming, setRenaming] = useState<string | null>(null);
  const renamingTeam: Team | undefined = teams.data?.find((t) => t.key === renaming);
  const [deleting, setDeleting] = useState<string | null>(null);
  const deletingTeam: Team | undefined = teams.data?.find((t) => t.key === deleting);
  return (
    <SettingsPage title="Teams">
      {teams.isPending ? (
        <SkeletonRows n={3} />
      ) : teams.isError ? (
        <ErrorState error={teams.error} onRetry={() => void teams.refetch()} title="Could not load teams" />
      ) : teams.data.length === 0 ? (
        <EmptyState title="No teams yet">
          <p>Create the first team with the form below.</p>
        </EmptyState>
      ) : (
        <SettingsCard>
          <div className={touchClass}>
            <table className={`${styles.table} ${styles.fitTable}`}>
              <caption className="sr-only">Teams</caption>
              <thead>
                <tr>
                  <th scope="col">Key</th>
                  <th scope="col">Name</th>
                  <th scope="col">Cycles</th>
                  {isAdmin && (
                    <th scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  )}
                </tr>
              </thead>
              <tbody>
                {teams.data.map((t) => (
                  <tr key={t.id}>
                    <td>
                      <code>{t.key}</code>
                    </td>
                    <td>{t.name}</td>
                    <td>
                      <TeamCyclesControl team={t} canEdit={isAdmin} onError={setCyclesError} />
                    </td>
                    {isAdmin && (
                      <td>
                        <button
                          type="button"
                          className={`${ui.btn} ${ui.ghost}`}
                          aria-label={`Rename team ${t.key}`}
                          onClick={() => setRenaming(t.key)}
                        >
                          Rename
                        </button>
                        <button
                          type="button"
                          className={`${ui.btn} ${ui.ghost}`}
                          aria-label={`Delete team ${t.key}`}
                          onClick={() => setDeleting(t.key)}
                        >
                          Delete
                        </button>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </SettingsCard>
      )}
      {cyclesError && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {cyclesError}
        </p>
      )}
      {teams.data && teams.data.length > 0 && (
        <p className={ui.hint}>
          Cycles are time boxes of work. When you turn cycles off, the cycles that exist stay and keep their
          dates and issues. The job stops: it makes no new cycle, completes none and moves no open issue on.
          The team&apos;s Cycles link leaves the sidebar. When you turn cycles on again, the job continues the
          numbers from the highest cycle.
        </p>
      )}

      {renamingTeam && <RenameTeamDialog team={renamingTeam} onClose={() => setRenaming(null)} />}

      {deletingTeam && <DeleteTeamDialog team={deletingTeam} onClose={() => setDeleting(null)} />}

      <TeamForm />
    </SettingsPage>
  );
}

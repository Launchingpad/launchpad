import { useState } from 'react';
import { useBackupSettings, useBackupStatus } from '../../../api/backups';
import { Spinner } from '../../../ui/States';
import { useIsPlatformAdmin } from '../access';
import { NoAccess, SettingsPage } from '../SettingsParts';
import { BackupChanges, BackupRuns } from './BackupHistory';
import { BackupDestinations } from './BackupDestinations';
import { BackupNotices } from './BackupNotices';
import { BackupRecipients } from './BackupRecipients';
import { BackupBanners, BackupStatusSection } from './BackupStatusSection';
import { loadErrorText } from './backupText';
import ui from '../../../styles/ui.module.css';

/**
 * Settings, Platform, Backups (LAUN-78; decisions-T16-backups.md #44). Only a platform admin sees it. Every
 * sentence is fixed text; no word of the server's error messages, of a remote host or of a run's `errorText`
 * reaches the screen, and no secret is ever rendered (a secret field shows only "Set" or "Not set").
 */
export function BackupsPage() {
  const isPlatformAdmin = useIsPlatformAdmin();
  // The nav item is hidden from other people, but a direct visit must still be refused.
  if (!isPlatformAdmin) {
    return (
      <NoAccess
        title="You need platform admin access."
        hint="Only the person who runs this install can open this page."
      />
    );
  }
  return <BackupsView />;
}

function BackupsView() {
  const status = useBackupStatus();
  const settings = useBackupSettings();
  // UI only: the API enforces just a non-empty set of active recipients (decisions #44).
  const [keysSaved, setKeysSaved] = useState(false);
  return (
    <SettingsPage
      title="Backups"
      description="Scheduled, encrypted copies of the database on storage you choose. Settings for the whole install."
      wide
    >
      {status.isPending && <Spinner label="Loading the backup status" />}
      {status.isError && (
        <div role="alert" className={`${ui.banner} ${ui.errorBanner}`} data-testid="load-error">
          <div>
            <strong>Could not load the backup status.</strong> {loadErrorText(status.error)}
          </div>
          <button type="button" className={ui.btn} onClick={() => void status.refetch()}>
            Try again
          </button>
        </div>
      )}
      {status.data && (
        <>
          <BackupBanners status={status.data} />
          <BackupStatusSection status={status.data} keysSaved={keysSaved} onKeysSaved={setKeysSaved} />
          <BackupRecipients />
          <BackupDestinations
            supportedTypes={status.data.supportedTypes}
            signingPublicSet={status.data.signingPublic !== ''}
            keysSaved={keysSaved}
          />
          {settings.data ? (
            <BackupNotices settings={settings.data} mailReady={status.data.mailReady} />
          ) : settings.isError ? (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              Could not load the notice settings. {loadErrorText(settings.error)}
            </p>
          ) : (
            <Spinner label="Loading the notice settings" />
          )}
          <BackupRuns />
          <BackupChanges />
        </>
      )}
    </SettingsPage>
  );
}

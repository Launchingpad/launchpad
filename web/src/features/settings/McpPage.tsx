import { useState } from 'react';
import { apiBase } from '../../env';
import { useOrgSlug } from '../../api/org';
import { useTeams } from '../../api/queries';
import { SettingsCard, SettingsPage, SettingsRow, SettingsSection } from './SettingsParts';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';

/**
 * How to connect an agent runtime to this org with the MCP server (an entry of `.mcp.json`; the key `launchpad` in the sample is only a local name, the user may change it).
 * It shows the connection values and a ready entry; it holds no secret, the token comes from the
 * environment (`LAUNCHPAD_TOKEN`) and is made on the API tokens page.
 */
export function McpPage() {
  const org = useOrgSlug();
  const teams = useTeams();
  const url = apiBase();
  const team = teams.data?.[0]?.key ?? '<your team key>';
  const entry = JSON.stringify(
    {
      mcpServers: {
        launchpad: {
          command: '/abs/path/to/launchpad-mcp',
          args: [],
          env: {
            LAUNCHPAD_URL: url,
            LAUNCHPAD_TOKEN: '${LAUNCHPAD_TOKEN}',
            LAUNCHPAD_ORG: org,
            LAUNCHPAD_TEAM: team,
          },
        },
      },
    },
    null,
    2,
  );
  const [note, setNote] = useState<string | null>(null);
  async function copy() {
    try {
      await navigator.clipboard.writeText(entry);
      setNote('Copied.');
    } catch {
      setNote('Could not copy. Select the text and copy it by hand.');
    }
  }
  return (
    <SettingsPage
      title="MCP"
      description="The MCP server lets an agent, such as Claude, read and change issues here with a token."
    >
      <SettingsSection title="Connection">
        <SettingsCard>
          <SettingsRow label="Server URL" description="LAUNCHPAD_URL">
            <code>{url}</code>
          </SettingsRow>
          <SettingsRow label="Organisation" description="LAUNCHPAD_ORG">
            <code>{org}</code>
          </SettingsRow>
          <SettingsRow
            label="Default team"
            description="The first team in the list. LAUNCHPAD_TEAM, optional."
          >
            <code>{team}</code>
          </SettingsRow>
        </SettingsCard>
      </SettingsSection>
      <SettingsSection
        title="Token"
        description="The MCP server signs in with a token of an agent. Keep it in the environment, not in a file."
      >
        <SettingsCard>
          <SettingsRow
            label="Make an agent"
            description="An agent is a user for a program. It cannot sign in."
            to={`/${org}/settings/agents`}
          />
          <SettingsRow
            label="Make a token"
            description="Read and write access, with an expiry. It is shown once."
            to={`/${org}/settings/tokens`}
          />
        </SettingsCard>
      </SettingsSection>
      <SettingsSection
        title=".mcp.json"
        description="Put this entry in the .mcp.json of your project. The name launchpad is only a local name: you may change it."
      >
        <SettingsCard padded>
          <pre className={styles.envBox} aria-label="MCP configuration">
            <code>{entry}</code>
          </pre>
          <div className={styles.actions}>
            <button type="button" className={ui.btn} onClick={() => void copy()}>
              Copy
            </button>
            <span role="status" className={ui.hint}>
              {note}
            </span>
          </div>
        </SettingsCard>
      </SettingsSection>
    </SettingsPage>
  );
}

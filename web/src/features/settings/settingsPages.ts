/**
 * The settings pages, in sidebar order (LAUN-26). The sidebar and its search both read this list. `keywords`
 * are the labels of the rows and sections on the page, so a search for "postmark" finds Platform.
 */
export type SettingsGroup = 'Account' | 'Administration' | 'Platform';

export interface SettingsPageDef {
  /** The path after `/:org/settings/`. */
  path: string;
  label: string;
  group: SettingsGroup;
  /** `member`: every member of the org (LAUN-36). `admin`: an org admin. `platform`: a platform admin. */
  access: 'member' | 'admin' | 'platform';
  keywords: readonly string[];
}

export const settingsPages: readonly SettingsPageDef[] = [
  {
    path: 'notifications',
    label: 'Notifications',
    group: 'Account',
    access: 'member',
    keywords: ['email', 'inbox', 'unsubscribe', 'follow a team', 'mentions', 'assigned', 'excerpt'],
  },
  {
    path: 'sessions',
    label: 'Sessions',
    group: 'Account',
    access: 'member',
    keywords: [
      'log out',
      'sign out',
      'log out everywhere else',
      'devices',
      'this device',
      'revoke a session',
    ],
  },
  {
    path: 'connected-accounts',
    label: 'Connected accounts',
    group: 'Account',
    access: 'member',
    keywords: [
      'git handle',
      'github username',
      'gitlab username',
      'bitbucket username',
      'link my account',
      'pull request author',
      'attribution',
    ],
  },
  {
    path: 'mobile-devices',
    label: 'Mobile devices',
    group: 'Account',
    access: 'member',
    keywords: ['phone', 'push', 'push notifications', 'remove a device', 'app', 'ios', 'android'],
  },
  {
    path: 'personal-webhooks',
    label: 'Personal webhooks',
    group: 'Account',
    access: 'member',
    keywords: ['webhook', 'my webhook', 'assigned to me', 'my issues', 'test message'],
  },
  {
    path: 'appearance',
    label: 'Appearance',
    group: 'Account',
    access: 'member',
    keywords: ['theme', 'default', 'linear', 'paper', 'terminal', 'dark mode', 'light mode', 'font', 'look'],
  },
  {
    path: 'setup',
    label: 'Get started',
    group: 'Administration',
    access: 'admin',
    keywords: ['get launchpad ready', 'checklist', 'first run', 'setup steps'],
  },
  {
    path: 'organisation',
    label: 'Organisation',
    group: 'Administration',
    access: 'admin',
    keywords: ['name', 'slug'],
  },
  {
    path: 'teams',
    label: 'Teams',
    group: 'Administration',
    access: 'admin',
    keywords: ['create a team', 'team key', 'team name', 'cycles', 'turn on cycles', 'sprints'],
  },
  {
    path: 'products',
    label: 'Products',
    group: 'Administration',
    access: 'admin',
    keywords: ['product', 'group teams', 'assign teams', 'create a product', 'archive a product'],
  },
  {
    path: 'agent-guidance',
    label: 'Agent guidance',
    group: 'Administration',
    access: 'admin',
    keywords: ['agents', 'instructions', 'conventions', 'mcp', 'team guidance', 'working rules'],
  },
  {
    path: 'chat-services',
    label: 'Chat services',
    group: 'Administration',
    access: 'admin',
    keywords: [
      'slack',
      'discord',
      'fluxer',
      'webhook',
      'incoming webhook',
      'channel',
      'custom webhook',
      'test message',
      'post events',
      'services',
    ],
  },
  {
    path: 'push',
    label: 'Mobile push',
    group: 'Administration',
    access: 'admin',
    keywords: [
      'push notifications',
      'relay',
      'relay url',
      'bearer secret',
      'apns',
      'fcm',
      'test push',
      'phone',
    ],
  },
  {
    path: 'project-statuses',
    label: 'Project statuses',
    group: 'Administration',
    access: 'admin',
    keywords: [
      'project status',
      'rename a status',
      'status colour',
      'reorder statuses',
      'kinds',
      'backlog',
      'planned',
      'paused',
    ],
  },
  {
    path: 'project-labels',
    label: 'Project labels',
    group: 'Administration',
    access: 'admin',
    keywords: ['label', 'labels for projects', 'create a label', 'group labels', 'colour', 'delete a label'],
  },
  {
    path: 'members',
    label: 'Members',
    group: 'Administration',
    access: 'admin',
    keywords: ['invites', 'invite a member', 'roles', 'email', 'team access'],
  },
  {
    path: 'signups',
    label: 'Sign-ups',
    group: 'Administration',
    access: 'admin',
    keywords: ['sign-up requests', 'approve', 'decline', 'unconfirmed'],
  },
  {
    path: 'agents',
    label: 'Agents',
    group: 'Administration',
    access: 'admin',
    keywords: ['create an agent', 'username', 'bot'],
  },
  {
    path: 'tokens',
    label: 'API tokens',
    group: 'Administration',
    access: 'admin',
    keywords: ['create a token', 'revoke', 'expiry', 'scopes'],
  },
  {
    path: 'integrations',
    label: 'Integrations',
    group: 'Administration',
    access: 'admin',
    keywords: [
      'git connections',
      'gitlab',
      'github',
      'bitbucket',
      'connect a git host',
      'webhook',
      'webhook secret',
      'rotate secret',
      'merge requests',
      'pull requests',
      'repositories',
      'map a repository',
      'access token',
    ],
  },
  {
    path: 'git-workflow',
    label: 'Git workflow',
    group: 'Administration',
    access: 'admin',
    keywords: [
      'state moves',
      'merged to done',
      'opened to in review',
      'reset to defaults',
      'merge request state',
      'pull request state',
      'workflow rules',
    ],
  },
  {
    path: 'mcp',
    label: 'MCP',
    group: 'Administration',
    access: 'admin',
    keywords: ['mcp server', 'claude', 'environment variables', 'LAUNCHPAD_URL', '.mcp.json'],
  },
  {
    path: 'import',
    label: 'Import from Linear',
    group: 'Administration',
    access: 'admin',
    keywords: [
      'linear',
      'linear api key',
      'export',
      'user map',
      'dry run',
      'verify',
      'migrate',
      'turn on cycles after import',
    ],
  },
  {
    path: 'platform',
    label: 'Platform',
    group: 'Platform',
    access: 'platform',
    keywords: ['mail', 'login email', 'postmark', 'smtp', 'sign-up mode', 'platform admins', 'test email'],
  },
  {
    path: 'move',
    label: 'Move server',
    group: 'Platform',
    access: 'platform',
    keywords: [
      'move file',
      'passphrase',
      'include attachments',
      'new server',
      'migrate',
      'export',
      'restore',
    ],
  },
  {
    path: 'backups',
    label: 'Backups',
    group: 'Platform',
    access: 'platform',
    keywords: [
      'off-site backups',
      'scheduled backups',
      'destinations',
      's3',
      'webdav',
      'nextcloud',
      'sftp',
      'dropbox',
      'encryption keys',
      'age',
      'recipients',
      'signing key',
      'restore',
      'stale alert',
    ],
  },
];

export interface SettingsHit {
  page: SettingsPageDef;
  /** The row or section label that matched, when the page name did not. */
  via?: string;
}

/** Pages whose name, or one of whose row labels, contains the text. An empty query finds nothing. */
export function searchSettings(pages: readonly SettingsPageDef[], query: string): SettingsHit[] {
  const q = query.trim().toLowerCase();
  if (!q) return [];
  const hits: SettingsHit[] = [];
  for (const page of pages) {
    if (page.label.toLowerCase().includes(q)) hits.push({ page });
    else {
      const via = page.keywords.find((k) => k.toLowerCase().includes(q));
      if (via) hits.push({ page, via });
    }
  }
  return hits;
}

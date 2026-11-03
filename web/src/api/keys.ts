import type { IssueFilters } from '../features/issues/filters';

export const keys = {
  session: ['session'] as const,
  platform: ['platform', 'settings'] as const,
  notificationSettings: (o: string) => ['org', o, 'notification-settings'] as const,
  issueSubscriptions: (o: string) => ['org', o, 'issue-subscription'] as const,
  issueSubscription: (o: string, issue: string) => ['org', o, 'issue-subscription', issue] as const,
  platformAdmins: ['platform', 'admins'] as const,
  backupStatus: ['platform', 'backups', 'status'] as const,
  backupSettings: ['platform', 'backups', 'settings'] as const,
  backupRecipients: ['platform', 'backups', 'recipients'] as const,
  backupDestinations: ['platform', 'backups', 'destinations'] as const,
  backupRuns: (destination: string) => ['platform', 'backups', 'runs', destination] as const,
  backupRun: (id: string) => ['platform', 'backups', 'run', id] as const,
  backupChanges: ['platform', 'backups', 'changes'] as const,
  orgList: ['orgs', 'list'] as const,
  org: (o: string) => ['org', o] as const,
  orgDetail: (o: string) => ['org', o, 'detail'] as const,
  onboarding: (o: string) => ['org', o, 'onboarding'] as const,
  /** The caller's own git handle per connection (LAUN-199). */
  gitAccounts: (o: string) => ['org', o, 'git-accounts'] as const,
  /** Every link of the org, for an org admin (LAUN-199). */
  gitIdentities: (o: string) => ['org', o, 'git-identities'] as const,
  agentGuidance: (o: string) => ['org', o, 'agent-guidance'] as const,
  /** LAUN-244: one team's git workflow rules; ['org', o, 'git-workflow'] is every team's. */
  gitWorkflow: (o: string, team: string) => ['org', o, 'git-workflow', team] as const,
  chat: (o: string) => ['org', o, 'chat-services'] as const,
  /** The org's push relay, for an org admin (LAUN-197). */
  push: (o: string) => ['org', o, 'push'] as const,
  /** The caller's own registered devices (LAUN-197). */
  pushDevices: (o: string) => ['org', o, 'push-devices'] as const,
  /** The caller's own personal webhooks (LAUN-229). */
  personalWebhooks: (o: string) => ['org', o, 'personal-webhooks'] as const,
  teams: (o: string) => ['org', o, 'teams'] as const,
  states: (o: string, team: string) => ['org', o, 'states', team] as const,
  products: (o: string) => ['org', o, 'products'] as const,
  labels: (o: string) => ['org', o, 'labels'] as const,
  /** The project labels (LAUN-194), not the issue labels. */
  projectLabels: (o: string) => ['org', o, 'project-labels'] as const,
  cycles: (o: string, team: string) => ['org', o, 'cycles', team] as const,
  projects: (o: string, team: string) => ['org', o, 'projects', team] as const,
  projectStatuses: (o: string) => ['org', o, 'project-statuses'] as const,
  projectsAll: (o: string) => ['org', o, 'projects-all'] as const,
  /** Every project detail; prefix of `project`. Issue changes refresh it: it carries the issue counts (LAUN-170). */
  projectDetails: (o: string) => ['org', o, 'project'] as const,
  milestones: (o: string, id: string) => ['org', o, 'milestones', id] as const,
  projectUpdates: (o: string, id: string) => ['org', o, 'project-updates', id] as const,
  project: (o: string, id: string) => ['org', o, 'project', id] as const,
  members: (o: string) => ['org', o, 'members'] as const,
  mySessions: ['me', 'sessions'] as const,
  /** The caller's display preferences (LAUN-235): install-wide, not per org. */
  myPreferences: ['me', 'preferences'] as const,
  tokens: (o: string) => ['org', o, 'tokens'] as const,
  invites: (o: string) => ['org', o, 'invites'] as const,
  signups: (o: string) => ['org', o, 'signups'] as const,
  gitIntegrations: (o: string) => ['org', o, 'git-integrations'] as const,
  gitIntegration: (o: string, id: string) => ['org', o, 'git-integrations', id] as const,
  gitRepos: (o: string, id: string) => ['org', o, 'git-integrations', id, 'repos'] as const,
  gitDeliveries: (o: string, id: string) => ['org', o, 'git-integrations', id, 'deliveries'] as const,
  gitAvailable: (o: string, id: string, q: string) =>
    ['org', o, 'git-integrations', id, 'available', q] as const,
  issues: (o: string) => ['org', o, 'issues'] as const,
  issueList: (o: string, f: Partial<IssueFilters> & Record<string, unknown>) =>
    ['org', o, 'issues', 'list', f] as const,
  issueSearch: (o: string, q: string, team = '') => ['org', o, 'issues', 'search', q, team] as const,
  issueDetails: (o: string) => ['org', o, 'issue'] as const,
  issue: (o: string, id: string) => ['org', o, 'issue', id.toUpperCase()] as const,
  // Under issueDetails, so every issue.* event that refreshes the details refreshes the activity too (LAUN-95).
  issueActivity: (o: string, id: string) => ['org', o, 'issue', id.toUpperCase(), 'activity'] as const,
  inboxAll: (o: string) => ['org', o, 'inbox'] as const,
  inbox: (o: string, f: string) => ['org', o, 'inbox', f] as const,
  unread: (o: string) => ['org', o, 'unread'] as const,
  commentsAll: (o: string) => ['org', o, 'comments'] as const,
  comments: (o: string, id: string) => ['org', o, 'comments', id.toUpperCase()] as const,
  importSession: (o: string) => ['org', o, 'import', 'session'] as const,
  importUsers: (o: string, s: string) => ['org', o, 'import', 'users', s] as const,
};

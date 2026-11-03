import type { components } from './schema';

type S = components['schemas'];
export type Issue = S['Issue'];
export type IssueList = S['IssueList'];
export type Team = S['Team'];
export type Product = S['Product'];
export type WorkflowState = S['WorkflowState'];
export type StateRef = S['StateRef'];
export type StateType = S['StateType'];
export type Label = S['Label'];
export type LabelRef = S['LabelRef'];
export type Member = S['Member'];
export type TeamAccessInput = S['TeamAccessInput']; // LAUN-227 (decisions-T20 #48, #74)
export type TeamAccessResult = S['TeamAccessResult'];
export type ApiToken = S['Token'];
/** One of the caller's own login sessions (LAUN-160). Not `Session`, which is the login answer. */
export type LoginSession = S['Session'];
export type CreatedToken = S['CreatedToken'];
export type TokenInput = S['TokenInput'];
export type AgentInput = S['AgentInput'];
export type PlatformSettings = S['PlatformSettings'];
export type PlatformSettingsInput = S['PlatformSettingsInput'];
export type MailTestResult = S['MailTestResult'];
export type UserRef = S['UserRef'];
export type IssueRef = S['IssueRef'];
export type Comment = S['Comment'];
export type Relations = S['Relations'];
export type Relation = S['Relation'];
export type RelationType = S['RelationType'];
export type StateChange = S['StateChange'];
export type Attachment = S['Attachment'];
export type ChangeRequest = S['ChangeRequest']; // LAUN-83: the Changes block of an issue
export type ActivityEntry = S['Activity']; // LAUN-49 / LAUN-83: GET /issues/{issue}/activity
export type ApiEvent = S['Event'];
export type UpdateIssueInput = S['UpdateIssueInput'];
export type CreateIssueInput = S['CreateIssueInput'];
export type Org = S['Org'];
export type UpdateOrgInput = S['UpdateOrgInput'];
export type GitAccount = S['GitAccount']; // LAUN-199
export type GitIdentity = S['GitIdentity'];
export type AgentGuidance = S['AgentGuidance'];
export type AgentGuidanceEntry = S['AgentGuidanceEntry'];
export type GitWorkflow = S['GitWorkflow']; // LAUN-244
export type GitWorkflowRule = S['GitWorkflowRule'];
export type GitWorkflowEvent = S['GitWorkflowEvent'];
export type GitWorkflowRuleInput = S['GitWorkflowRuleInput'];
export type ChatService = S['ChatService']; // LAUN-204
export type ChatServiceList = S['ChatServiceList'];
export type ChatServiceCreate = S['ChatServiceCreate'];
export type ChatServicePatch = S['ChatServicePatch'];
export type ChatTestResult = S['ChatTestResult'];
export type PushRelay = S['PushRelay']; // LAUN-197
export type PushRelayInput = S['PushRelayInput'];
export type PushTestResult = S['PushTestResult'];
export type PushDevice = S['PushDevice'];
export type PushDevices = S['PushDevices'];
export type PersonalWebhook = S['PersonalWebhook']; // LAUN-229
export type PersonalWebhookList = S['PersonalWebhookList'];
export type PersonalWebhookCreate = S['PersonalWebhookCreate'];
export type PersonalWebhookPatch = S['PersonalWebhookPatch'];
export type Onboarding = S['Onboarding'];
export type OnboardingStep = S['OnboardingStep'];
export type OnboardingStepId = S['OnboardingStepId'];
export type Cycle = S['Cycle'];
export type UpdateCycleInput = S['UpdateCycleInput']; // LAUN-172
export type Project = S['Project'];
export type ProjectStatus = S['ProjectStatus']; // LAUN-195
export type ProjectStatusRef = S['ProjectStatusRef'];
export type ProjectLabel = S['ProjectLabel']; // LAUN-194
export type ProjectLabelRef = S['ProjectLabelRef'];
export type CreateProjectInput = S['CreateProjectInput']; // LAUN-166
export type UpdateProjectInput = S['UpdateProjectInput'];
export type PreparedUpload = S['PreparedUpload'];

export type Session = Pick<S['LoginResponse'], 'user' | 'orgs'>;
export type SessionUser = S['LoginUser'];
// First-run setup. The setup page is features/auth/SetupPage.tsx.
export type SetupStatus = S['SetupStatus'];
export type SetupRequest = S['SetupRequest'];
export type SetupResponse = S['SetupResponse'];
export type SessionOrg = S['LoginOrg'];

// Invites, sign-up requests and platform admins (server and web).
export type SignupMode = S['SignupMode'];
export type Invite = S['Invite'];
export type InviteInput = S['InviteInput'];
export type InviteRole = S['InviteRole'];
export type CreatedInvite = S['CreatedInvite'];
export type InviteAcceptInput = S['InviteAcceptInput'];
export type InviteAcceptResponse = S['InviteAcceptResponse'];
export type SignupRequest = S['SignupRequest'];
export type SignupApproveInput = S['SignupApproveInput'];
export type SignupCreateRequest = S['SignupCreateRequest'];
export type SignupRejectInput = S['SignupRejectInput'];
export type PlatformAdmin = S['PlatformAdmin'];

// Notifications (LAUN-23 design, LAUN-35 API, LAUN-37 web inbox).
export type InboxNotification = S['InboxNotification'];
export type InboxNotificationList = S['InboxNotificationList'];

// Notification settings (LAUN-32 server, LAUN-36 web).
export type NotificationSettings = S['NotificationSettings'];
export type NotificationSettingsInput = S['NotificationSettingsInput'];
export type NotificationTeamSettingsInput = S['NotificationTeamSettingsInput'];
// Issue subscription (LAUN-38).
export type IssueSubscription = S['IssueSubscription'];
export type NotificationEvent = S['NotificationEvent'];
export type NotificationEventSettings = S['NotificationEventSettings'];
export type NotificationValue = S['NotificationValue'];
export type NotificationSource = S['NotificationSource'];
export type NotificationEventInput = S['NotificationEventInput'];

// LAUN-9: git connections
export type GitIntegration = S['GitIntegration'];
export type GitIntegrationCreated = S['GitIntegrationCreated'];
export type GitIntegrationCheck = S['GitIntegrationCheck'];
export type CreateGitIntegrationInput = S['CreateGitIntegrationInput'];
export type UpdateGitIntegrationInput = S['UpdateGitIntegrationInput'];
export type GitRepo = S['GitRepo'];
export type CreateGitRepoInput = S['CreateGitRepoInput'];
export type HookRefreshResult = S['HookRefreshResult']; // LAUN-83
export type BackfillResult = S['BackfillResult']; // LAUN-83
export type AvailableRepo = S['AvailableRepo'];
export type GitRotateResult = S['GitRotateResult'];
export type GitDelivery = S['GitDelivery']; // LAUN-87
export type GitDeliverySummary = S['GitDeliverySummary'];

// LAUN-66: products (decisions-T18). `Product` is above (LAUN-64).
export type ProductDetail = S['ProductDetail'];
export type CreateProductInput = S['CreateProductInput'];
export type UpdateProductInput = S['UpdateProductInput'];

// LAUN-10: the Linear import (decisions-T17 #36 to #52).
export type ImportSession = S['ImportSession'];
export type ImportJob = S['ImportJob'];
export type ImportUsers = S['ImportUsers'];
export type ImportExportUser = S['ImportExportUser'];
export type ImportUserMap = S['ImportUserMap'];
export type ImportReport = S['ImportReport'];

// Scheduled off-site backups (LAUN-78; server LAUN-69, LAUN-75).
export type BackupStatus = S['BackupStatus'];
export type BackupSettings = S['BackupSettings'];
export type BackupSettingsInput = S['BackupSettingsInput'];
export type BackupRecipient = S['BackupRecipient'];
export type BackupRecipientList = S['BackupRecipientList'];
export type BackupDestination = S['BackupDestination'];
export type BackupDestinationType = S['BackupDestinationType'];
export type CreateBackupDestinationInput = S['CreateBackupDestinationInput'];
export type UpdateBackupDestinationInput = S['UpdateBackupDestinationInput'];
export type BackupRun = S['BackupRun'];
export type BackupChange = S['BackupChange'];
export type BackupProbeInput = S['BackupProbeInput'];
export type BackupUnsavedTestInput = S['BackupUnsavedTestInput'];

/** LAUN-235: the caller's display preferences (the theme). */
export type UserPreferences = S['UserPreferences'];
export type UiTheme = S['UiTheme'];

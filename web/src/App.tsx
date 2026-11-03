import { Link, Navigate, Route, Routes, useParams } from 'react-router-dom';
import { OrgProvider } from './api/org';
import { LoginPage } from './features/auth/LoginPage';
import { InvitePage } from './features/auth/InvitePage';
import { SetupPage } from './features/auth/SetupPage';
import { SignupPage } from './features/auth/SignupPage';
import { RequireAuth, useAuthedSession } from './features/auth/session';
import { CycleListPage } from './features/cycles/CycleListPage';
import { CyclePage } from './features/cycles/CyclePage';
import { CycleJump } from './features/cycles/CycleJump';
import { ProjectListPage } from './features/projects/ProjectListPage';
import { ProjectPage } from './features/projects/ProjectPage';
import { InboxPage } from './features/inbox/InboxPage';
import { IssueDetailPage } from './features/issues/IssueDetailPage';
import { IssuesPage } from './features/issues/IssuesPage';
import { ProductPage } from './features/issues/ProductPage';
import { AgentsPage } from './features/settings/AgentsPage';
import { IntegrationPage } from './features/settings/git/IntegrationPage';
import { IntegrationsPage } from './features/settings/git/IntegrationsPage';
import { GitWorkflowPage } from './features/settings/git/GitWorkflowPage';
import { ImportPage } from './features/settings/ImportPage';
import { McpPage } from './features/settings/McpPage';
import { MembersPage } from './features/settings/MembersPage';
import { NotificationsPage } from './features/settings/NotificationsPage';
import { OrganisationPage } from './features/settings/OrganisationPage';
import { ConnectedAccountsPage } from './features/settings/ConnectedAccountsPage';
import { AgentGuidancePage } from './features/settings/AgentGuidancePage';
import { ChatServicesPage } from './features/settings/ChatServicesPage';
import { PushPage } from './features/settings/PushPage';
import { MobileDevicesPage } from './features/settings/MobileDevicesPage';
import { PersonalWebhooksPage } from './features/settings/PersonalWebhooksPage';
import { AppearancePage } from './features/settings/AppearancePage';
import { ProductsPage } from './features/settings/ProductsPage';
import { ProjectStatusesPage } from './features/settings/ProjectStatusesPage';
import { ProjectLabelsPage } from './features/settings/ProjectLabelsPage';
import { PlatformPage } from './features/settings/PlatformPage';
import { BackupsPage } from './features/settings/backups/BackupsPage';
import { SettingsIndex, SettingsLayout } from './features/settings/SettingsLayout';
import { SetupChecklistPage } from './features/settings/SetupChecklistPage';
import { SignupsPage } from './features/settings/SignupsPage';
import { TeamsPage } from './features/settings/TeamsPage';
import { SessionsPage } from './features/settings/SessionsPage';
import { TokensPage } from './features/settings/TokensPage';
import { UnsubscribePage } from './features/auth/UnsubscribePage';
import { Shell } from './features/shell/Shell';
import { UiProvider } from './features/shell/ui-context';
import { useAppTitle, useDocumentTitle, usePageTitle } from './lib/documentTitle';
import { EmptyState } from './ui/States';

function lastOrg(): string | null {
  try {
    return localStorage.getItem('devops-last-org');
  } catch {
    return null;
  }
}

/** An unknown address inside an org. */
function NotFound() {
  usePageTitle('Not found');
  return <EmptyState title="Page not found" />;
}

function NoOrgs() {
  useAppTitle('No organisation');
  return (
    <EmptyState title="You are not in any organisation yet">
      <p>Ask an admin to add you, then reload.</p>
    </EmptyState>
  );
}

function RootRedirect() {
  const { orgs } = useAuthedSession();
  if (orgs.length === 0) {
    return <NoOrgs />;
  }
  const slug = orgs.find((o) => o.slug === lastOrg())?.slug ?? orgs[0]!.slug;
  return <Navigate to={`/${slug}/issues`} replace />;
}

function OrgLayout() {
  const { org = '' } = useParams();
  const { orgs } = useAuthedSession();
  const member = orgs.some((o) => o.slug === org);
  useDocumentTitle(member ? [] : ['Not found']);
  if (!member) {
    return (
      <EmptyState title="Organisation not found">
        <p>
          You are not a member of “{org.slice(0, 60)}”. <Link to="/">Go to your organisations</Link>
        </p>
      </EmptyState>
    );
  }
  return (
    <OrgProvider slug={org}>
      <UiProvider>
        <Shell />
      </UiProvider>
    </OrgProvider>
  );
}

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/setup" element={<SetupPage />} />
      <Route path="/signup" element={<SignupPage />} />
      <Route path="/invite" element={<InvitePage />} />
      <Route path="/unsubscribe/:token" element={<UnsubscribePage />} />
      <Route element={<RequireAuth />}>
        <Route path="/" element={<RootRedirect />} />
        <Route path="/:org" element={<OrgLayout />}>
          <Route index element={<Navigate to="issues" replace />} />
          <Route path="issues" element={<IssuesPage scope="all" />} />
          <Route path="inbox" element={<InboxPage />} />
          <Route path="my-issues" element={<IssuesPage scope="mine" />} />
          <Route path="team/:team" element={<IssuesPage scope="team" />} />
          <Route path="team/:team/cycles" element={<CycleListPage />} />
          <Route path="team/:team/cycles/current" element={<CycleJump which="current" />} />
          <Route path="team/:team/cycles/upcoming" element={<CycleJump which="upcoming" />} />
          <Route path="team/:team/cycle/:number" element={<CyclePage />} />
          <Route path="projects" element={<ProjectListPage />} />
          <Route path="projects/:project" element={<ProjectPage />} />
          <Route path="product/:slug" element={<ProductPage />} />
          <Route path="issue/:identifier" element={<IssueDetailPage />} />
          <Route path="settings" element={<SettingsLayout />}>
            <Route index element={<SettingsIndex />} />
            <Route path="notifications" element={<NotificationsPage />} />
            <Route path="sessions" element={<SessionsPage />} />
            <Route path="connected-accounts" element={<ConnectedAccountsPage />} />
            <Route path="setup" element={<SetupChecklistPage />} />
            <Route path="organisation" element={<OrganisationPage />} />
            <Route path="teams" element={<TeamsPage />} />
            <Route path="products" element={<ProductsPage />} />
            <Route path="agent-guidance" element={<AgentGuidancePage />} />
            <Route path="chat-services" element={<ChatServicesPage />} />
            {/* The Slack page of LAUN-198 became Chat services (LAUN-204): old links and bookmarks still work. */}
            <Route path="slack" element={<Navigate to="../chat-services" replace />} />
            <Route path="push" element={<PushPage />} />
            <Route path="mobile-devices" element={<MobileDevicesPage />} />
            <Route path="personal-webhooks" element={<PersonalWebhooksPage />} />
            <Route path="appearance" element={<AppearancePage />} />
            <Route path="project-statuses" element={<ProjectStatusesPage />} />
            <Route path="project-labels" element={<ProjectLabelsPage />} />
            <Route path="members" element={<MembersPage />} />
            <Route path="signups" element={<SignupsPage />} />
            <Route path="agents" element={<AgentsPage />} />
            <Route path="tokens" element={<TokensPage />} />
            <Route path="integrations" element={<IntegrationsPage />} />
            <Route path="integrations/:integration" element={<IntegrationPage />} />
            <Route path="git-workflow" element={<GitWorkflowPage />} />
            <Route path="mcp" element={<McpPage />} />
            <Route path="import" element={<ImportPage />} />
            <Route path="platform" element={<PlatformPage />} />
            <Route path="backups" element={<BackupsPage />} />
            <Route path="*" element={<NotFound />} />
          </Route>
          <Route path="*" element={<NotFound />} />
        </Route>
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

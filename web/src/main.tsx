import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
// Self-hosted brand fonts (bundled into dist by Vite — no CDN, works offline).
import "@fontsource-variable/outfit";
import "@fontsource-variable/inter";
import "@fontsource-variable/jetbrains-mono";
// Design tokens MUST load before styles.css so the legacy --bg/--fg/--accent
// aliases resolve to the themed --lc-* tokens.
import "./tokens.css";
import "./styles.css";
import AgentsView from "./pages/AgentsView";
import RunView from "./pages/RunView";
import MemoryView from "./pages/MemoryView";
import InterruptInbox from "./pages/InterruptInbox";
import SnapshotsView from "./pages/SnapshotsView";
import AuditView from "./pages/AuditView";
import UsageView from "./pages/UsageView";
import AccessView from "./pages/AccessView";
import ActivityMonitor from "./pages/ActivityMonitor";
import LibraryView from "./pages/LibraryView";
import IntegrationsView from "./pages/IntegrationsView";
import VolumesView from "./pages/VolumesView";
import PathTreeView from "./pages/PathTreeView";
import DocumentsView from "./pages/DocumentsView";
import ChannelsView from "./pages/ChannelsView";
import SchedulesView from "./pages/SchedulesView";
import TeamsView from "./pages/TeamsView";
import SectionPage, { SECTION_IDS, SettingsRedirect } from "./pages/SectionPages";
import MyCredentialsView from "./pages/MyCredentialsView";
import Layout from "./components/Layout";
import AgentIdRedirect from "./components/AgentIdRedirect";
import LoginView from "./pages/LoginView";
import LogoutView from "./pages/LogoutView";
import InviteView from "./pages/InviteView";
import ResetView from "./pages/ResetView";
import PendingView from "./pages/PendingView";
import DeviceView from "./pages/DeviceView";
import AccountView from "./pages/AccountView";
import { installUIRequestHeader } from "./lib/uiSession";

// Before anything fetches: every same-origin request carries the header the
// server's CSRF check asks of a session-authenticated write.
installUIRequestHeader(window);

// Mounted at /ui/ in production; the BrowserRouter basename matches
// so links / navigation generate correct paths.
//
// Routing notes (v0.8.20 split-view refactor):
//   / (index) → redirect to /agents
//   /agents → split-view tree + detail (selection via ?agent=)
//   /agents/:agentId → legacy redirect to /agents?agent=:agentId
ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter basename="/ui">
      <Routes>
        {/* Login is outside the Layout shell (no nav, no whoami gate) —
            it's where a 401 bounces and where the token-entry form lives. */}
        <Route path="login" element={<LoginView />} />
        {/* Logout is a page, not a server GET: it posts the logout and then
            goes to the login page, so a link elsewhere cannot end a session. */}
        <Route path="logout" element={<LogoutView />} />
        {/* The password gate's invitation and reset links (RFC FA P2): public
            pages, like login, whose code rides the URL fragment. */}
        <Route path="invite" element={<InviteView />} />
        <Route path="reset" element={<ResetView />} />
        <Route path="pending" element={<PendingView />} />
        {/* Device login approval (RFC FA P5): public like login; it sends a
            signed-out visitor to the login page and back. */}
        <Route path="device" element={<DeviceView />} />
        <Route path="/" element={<Layout />}>
          <Route index element={<Navigate to="/agents" replace />} />
          <Route path="run" element={<RunView />} />
          <Route path="my-credentials" element={<MyCredentialsView />} />
          <Route path="account" element={<AccountView />} />
          <Route path="teams" element={<TeamsView />} />
          <Route path="agents" element={<AgentsView />} />
          <Route path="agents/:agentId" element={<AgentIdRedirect />} />
          <Route path="library" element={<LibraryView />}>
            <Route index element={<Navigate to="/library/agents" replace />} />
          </Route>
          <Route path="library/agents" element={<LibraryView />} />
          <Route path="library/skills" element={<LibraryView />} />
          <Route path="library/mcp-servers" element={<LibraryView />} />
          <Route path="library/hooks" element={<LibraryView />} />
          <Route path="integrations" element={<IntegrationsView />}>
            <Route
              index
              element={<Navigate to="/integrations/webhooks" replace />}
            />
          </Route>
          <Route path="integrations/webhooks" element={<IntegrationsView />} />
          <Route
            path="integrations/a2a-server-cards"
            element={<IntegrationsView />}
          />
          <Route path="integrations/a2a-agents" element={<IntegrationsView />} />
          <Route
            path="integrations/memory-backends"
            element={<IntegrationsView />}
          />
          <Route path="volumes" element={<VolumesView />}>
            <Route index element={<Navigate to="/volumes/persistent" replace />} />
          </Route>
          <Route path="volumes/persistent" element={<VolumesView />} />
          <Route path="volumes/ephemeral" element={<VolumesView />} />
          {/* The Path tree is shown as Documents; /paths is its old address. */}
          <Route path="documents" element={<PathTreeView />} />
          <Route path="paths" element={<Navigate to="/documents" replace />} />
          <Route path="documents/:documentId" element={<DocumentsView />} />
          <Route path="channels" element={<ChannelsView />} />
          <Route path="schedules" element={<SchedulesView />} />
          <Route path="memory" element={<MemoryView />} />
          <Route path="interrupts" element={<InterruptInbox />} />
          <Route path="snapshots" element={<SnapshotsView />} />
          <Route path="audit" element={<AuditView />} />
          <Route path="usage" element={<UsageView />} />
          {/* Users, access control and tokens are sections of /access; their
              old addresses (the settings menu's Users, Settings → Access and
              Tokens) still land on them. */}
          <Route path="access/:section?" element={<AccessView />} />
          <Route path="users" element={<Navigate to="/access/users" replace />} />
          <Route path="settings/access" element={<Navigate to="/access/groups" replace />} />
          <Route path="settings/tokens" element={<Navigate to="/access/tokens" replace />} />
          <Route path="activity" element={<ActivityMonitor />} />
          {/* The Settings hub's sections are pages of the left nav; its old
              addresses, /settings/<section>, land on them. */}
          {SECTION_IDS.map((id) => (
            <Route key={id} path={id} element={<SectionPage id={id} />} />
          ))}
          <Route path="settings/:section?" element={<SettingsRedirect />} />
          <Route path="*" element={<Navigate to="/agents" replace />} />
        </Route>
      </Routes>
    </BrowserRouter>
  </React.StrictMode>
);

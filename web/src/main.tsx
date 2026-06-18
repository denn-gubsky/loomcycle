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
import ActivityMonitor from "./pages/ActivityMonitor";
import LibraryView from "./pages/LibraryView";
import IntegrationsView from "./pages/IntegrationsView";
import VolumesView from "./pages/VolumesView";
import ChannelsView from "./pages/ChannelsView";
import SchedulesView from "./pages/SchedulesView";
import Layout from "./components/Layout";
import AgentIdRedirect from "./components/AgentIdRedirect";
import LoginView from "./pages/LoginView";

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
        <Route path="/" element={<Layout />}>
          <Route index element={<Navigate to="/agents" replace />} />
          <Route path="run" element={<RunView />} />
          <Route path="agents" element={<AgentsView />} />
          <Route path="agents/:agentId" element={<AgentIdRedirect />} />
          <Route path="library" element={<LibraryView />}>
            <Route index element={<Navigate to="/library/agents" replace />} />
          </Route>
          <Route path="library/agents" element={<LibraryView />} />
          <Route path="library/skills" element={<LibraryView />} />
          <Route path="library/mcp-servers" element={<LibraryView />} />
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
          <Route path="channels" element={<ChannelsView />} />
          <Route path="schedules" element={<SchedulesView />} />
          <Route path="memory" element={<MemoryView />} />
          <Route path="interrupts" element={<InterruptInbox />} />
          <Route path="snapshots" element={<SnapshotsView />} />
          <Route path="audit" element={<AuditView />} />
          <Route path="activity" element={<ActivityMonitor />} />
          <Route path="*" element={<Navigate to="/agents" replace />} />
        </Route>
      </Routes>
    </BrowserRouter>
  </React.StrictMode>
);

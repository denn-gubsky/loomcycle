import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import "./styles.css";
import AgentsView from "./pages/AgentsView";
import MemoryView from "./pages/MemoryView";
import InterruptInbox from "./pages/InterruptInbox";
import SnapshotsView from "./pages/SnapshotsView";
import AuditView from "./pages/AuditView";
import ActivityMonitor from "./pages/ActivityMonitor";
import LibraryView from "./pages/LibraryView";
import ChannelsView from "./pages/ChannelsView";
import Layout from "./components/Layout";
import AgentIdRedirect from "./components/AgentIdRedirect";

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
        <Route path="/" element={<Layout />}>
          <Route index element={<Navigate to="/agents" replace />} />
          <Route path="agents" element={<AgentsView />} />
          <Route path="agents/:agentId" element={<AgentIdRedirect />} />
          <Route path="library" element={<LibraryView />}>
            <Route index element={<Navigate to="/library/agents" replace />} />
          </Route>
          <Route path="library/agents" element={<LibraryView />} />
          <Route path="library/skills" element={<LibraryView />} />
          <Route path="library/mcp-servers" element={<LibraryView />} />
          <Route path="channels" element={<ChannelsView />} />
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

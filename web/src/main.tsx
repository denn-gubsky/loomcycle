import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import "./styles.css";
import RunList from "./pages/RunList";
import AgentDetail from "./pages/AgentDetail";
import MemoryView from "./pages/MemoryView";
import InterruptInbox from "./pages/InterruptInbox";
import Layout from "./components/Layout";

// Mounted at /ui/ in production; the BrowserRouter basename matches
// so links / navigation generate correct paths.
ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter basename="/ui">
      <Routes>
        <Route path="/" element={<Layout />}>
          <Route index element={<RunList />} />
          <Route path="agents/:agentId" element={<AgentDetail />} />
          <Route path="memory" element={<MemoryView />} />
          <Route path="interrupts" element={<InterruptInbox />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </BrowserRouter>
  </React.StrictMode>
);

import { Navigate, useParams } from "react-router-dom";

// AgentIdRedirect keeps legacy /agents/:agentId deep links working
// after the v0.8.20 split-view refactor moved the detail UI into
// /agents?agent=:id. Bookmarks survive; one-shot client-side
// redirect; no server-side route change required.
export default function AgentIdRedirect() {
  const { agentId } = useParams<{ agentId: string }>();
  if (!agentId) return <Navigate to="/agents" replace />;
  return <Navigate to={`/agents?agent=${encodeURIComponent(agentId)}`} replace />;
}

import { useParams } from "react-router-dom";
import AgentDetailPane from "../components/AgentDetailPane";

// AgentDetail is the route component for /agents/:agentId. Reads the
// agent id from the URL and delegates rendering to AgentDetailPane.
// Kept as a thin shell so a future split-view layout can mount
// AgentDetailPane inside a side-by-side container while standalone
// deep links still work.
export default function AgentDetail() {
  const { agentId } = useParams<{ agentId: string }>();
  if (!agentId) {
    return <div className="empty">Missing agent id in URL.</div>;
  }
  return <AgentDetailPane agentId={agentId} />;
}

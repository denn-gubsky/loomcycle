import { CredentialsPanel } from "../components/CredentialsPanel";

// MyCredentialsView is the standalone RFC CN "My Credentials" page: a user's OWN
// scope=user API tokens (a personal Slack/Telegram bot token, a per-user webhook
// secret), reachable by ANY authenticated login — including an isolated
// substrate:user user, who has no access to the tenant credentials page. It renders
// the shared CredentialsPanel in userOnly mode (scope=user only), so it offers
// exactly what the server permits a user to self-serve; the tenant credentials
// page (tenant + user authoring) stays separate.
export default function MyCredentialsView() {
  return (
    <div className="settings-view">
      <div className="settings-body">
        <CredentialsPanel userOnly />
      </div>
    </div>
  );
}

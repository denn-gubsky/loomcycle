import { Principal } from "../api";

// A server in open mode — no auth token, no config principals and no admin
// operator token — serves
// every route, token minting included, to anyone who can reach it. Everything
// in this UI works, so nothing else would tell the operator; this banner sits
// above every page until auth is configured.
export default function OpenModeBanner({ principal }: { principal: Principal | null | undefined }) {
  if (!principal?.open_mode) return null;
  return (
    <div className="open-mode-banner" role="alert">
      <strong>Unauthenticated:</strong> this server runs in open mode, so anyone who can reach it can use it.
      Set <code>LOOMCYCLE_AUTH_TOKEN</code> or declare <code>principals:</code> in the config, and restart the server.
    </div>
  );
}

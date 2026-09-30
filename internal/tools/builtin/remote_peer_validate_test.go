package builtin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// remotePeerTestHosts is the LOOMCYCLE_HTTP_HOST_ALLOWLIST the def tool
// fixtures run under: every host their tests author a remote peer at.
var remotePeerTestHosts = []string{"example.com", "peer.example", "peer2.example", "internal.example", "localhost", "93.184.216.34"}

// authorRemoteDef runs one create or fork of a remote peer def through the real
// def tool, as an admin — the shape checks it exercises apply to every author,
// and the fork keeps the static parent's api_key_env. kind is "memory" (a kind:remote MemoryBackendDef) or
// "document" (a DocumentSourceDef); fork derives from the fixture's static
// "primary".
func authorRemoteDef(t *testing.T, kind, op, baseURL, tenancy string) tools.Result {
	t.Helper()
	name := "peer"
	if op == "fork" {
		name = "primary"
	}
	overlay := fmt.Sprintf(`"config":{"base_url":%q}`, baseURL)
	if tenancy != "" {
		overlay += `,"tenancy_strategy":` + tenancy
	}
	call := json.RawMessage(fmt.Sprintf(`{"op":%q,"name":%q,"overlay":{%s}}`, op, name, overlay))
	switch kind {
	case "memory":
		call = json.RawMessage(fmt.Sprintf(`{"op":%q,"name":%q,"overlay":{"kind":"remote",%s}}`, op, name, overlay))
		tool, ctx, cleanup := memoryBackendDefFixture(t)
		defer cleanup()
		res, _ := tool.Execute(asAdmin(ctx), call)
		return res
	case "document":
		tool, ctx, cleanup := documentSourceDefFixture(t)
		defer cleanup()
		res, _ := tool.Execute(asAdmin(ctx), call)
		return res
	}
	t.Fatalf("unknown def kind %q", kind)
	return tools.Result{}
}

func TestRemoteDefAuthoring_RefusesAPrivateIPLiteralBaseURL(t *testing.T) {
	refused := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[::ffff:169.254.169.254]/",
		"http://127.0.0.1:8787",
		"http://[::1]:8787",
		"http://0.0.0.0:80",
		"http://[::]:80",
		"http://10.0.0.5",
		"http://172.16.3.4:8080",
		"https://192.168.1.10",
		"http://[fd00::1]/",
		"http://[fe80::1%25en0]/",
	}
	accepted := []string{
		"https://peer.example.com",
		"http://93.184.216.34:8787",
		// A hostname is NOT resolved at authoring; the dial guard decides.
		"http://localhost:8787",
	}
	for _, kind := range []string{"memory", "document"} {
		for _, op := range []string{"create", "fork"} {
			for _, u := range refused {
				t.Run(kind+"/"+op+"/refuse/"+u, func(t *testing.T) {
					res := authorRemoteDef(t, kind, op, u, "")
					if !res.IsError {
						t.Fatalf("%s %s with base_url %s succeeded; want it refused", kind, op, u)
					}
					if !strings.Contains(res.Text, "LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST") {
						t.Errorf("refusal should say how to reach a private peer; got %s", res.Text)
					}
				})
			}
			for _, u := range accepted {
				t.Run(kind+"/"+op+"/accept/"+u, func(t *testing.T) {
					if res := authorRemoteDef(t, kind, op, u, ""); res.IsError {
						t.Errorf("%s %s with base_url %s refused: %s", kind, op, u, res.Text)
					}
				})
			}
		}
	}
}

func TestRemoteDefAuthoring_RefusesAnUnsafeEnvPattern(t *testing.T) {
	refused := []string{
		"PEER_{tenant_id}_KEY",            // outside LOOMCYCLE_: never resolvable for a real tenant
		"BRAVE_API_KEY{tenant_id}",        // an exact third-party name, not a pattern family
		"LOOMCYCLE_AUTH_TOKEN{tenant_id}", // the shared tenant resolves the operator bearer
		"LOOMCYCLE_SECRET{tenant_id}_KEY", // the shared tenant resolves the credential master key
		"{tenant_id}",                     // the tenant id alone
	}
	accepted := []string{
		"LOOMCYCLE_PEER_{tenant_id}_KEY",
		"LOOMCYCLE_{tenant_id}_KEY",
	}
	const url = "https://peer.example.com"
	for _, kind := range []string{"memory", "document"} {
		for _, op := range []string{"create", "fork"} {
			for _, p := range refused {
				t.Run(kind+"/"+op+"/refuse/"+p, func(t *testing.T) {
					res := authorRemoteDef(t, kind, op, url, fmt.Sprintf(`{"kind":"key_per_tenant","env_pattern":%q}`, p))
					if !res.IsError {
						t.Fatalf("%s %s with env_pattern %s succeeded; want it refused", kind, op, p)
					}
					if !strings.Contains(res.Text, "env_pattern") {
						t.Errorf("refusal should name env_pattern; got %s", res.Text)
					}
				})
			}
			for _, p := range accepted {
				t.Run(kind+"/"+op+"/accept/"+p, func(t *testing.T) {
					res := authorRemoteDef(t, kind, op, url, fmt.Sprintf(`{"kind":"key_per_tenant","env_pattern":%q}`, p))
					if res.IsError {
						t.Errorf("%s %s with env_pattern %s refused: %s", kind, op, p, res.Text)
					}
				})
			}
		}
	}
}

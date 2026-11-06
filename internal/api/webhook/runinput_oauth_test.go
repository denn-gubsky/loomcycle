package webhook

import "testing"

// A sender that put an $oauth: reference in its body would choose which of
// the tenant's connected credentials is sent, exactly as with $cred:.
func TestWebhookPayloadCredentialRef_RefusesOAuthRef(t *testing.T) {
	for _, v := range []string{"$oauth:docs", "Bearer $oauth:docs"} {
		name, bad := payloadCredentialRef(map[string]string{"user_credentials.peer": v})
		if !bad || name != "peer" {
			t.Errorf("%q: (%q, %t), want the reference refused", v, name, bad)
		}
	}
	if _, bad := payloadCredentialRef(map[string]string{"user_credentials.peer": "a literal"}); bad {
		t.Error("a literal was refused")
	}
}

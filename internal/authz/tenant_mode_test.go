package authz

import "testing"

// RFC EZ P5: the modes are ordered by what they refuse, and a tenant's entry
// only ever raises its mode above the deployment's.
func TestTenantMode_AnEntryOnlyRaisesTheDeploymentMode(t *testing.T) {
	if !(ModeShadow.Strictness() < ModeCompat.Strictness() && ModeCompat.Strictness() < ModeEnforce.Strictness()) {
		t.Fatal("strictness must order shadow < compat < enforce: shadow honours no deny, compat does")
	}
	modes := map[string]Mode{"up": ModeEnforce, "same": ModeCompat, "down": ModeShadow, "mid": ModeCompat}
	for _, tc := range []struct {
		deployment Mode
		tenant     string
		want       Mode
	}{
		{ModeCompat, "up", ModeEnforce},
		{ModeCompat, "same", ModeCompat},
		{ModeCompat, "down", ModeCompat}, // below the floor: ignored, never applied
		{ModeCompat, "unlisted", ModeCompat},
		{ModeCompat, "", ModeCompat}, // a tenantless principal: the deployment's
		{ModeShadow, "mid", ModeCompat},
		{ModeShadow, "up", ModeEnforce},
		{ModeShadow, "unlisted", ModeShadow},
		{ModeEnforce, "down", ModeEnforce},
		{ModeEnforce, "mid", ModeEnforce},
	} {
		if got := TenantMode(tc.deployment, modes, tc.tenant); got != tc.want {
			t.Errorf("TenantMode(%s, %q) = %s, want %s", tc.deployment, tc.tenant, got, tc.want)
		}
	}
	if got := TenantMode(ModeShadow, nil, "any"); got != ModeShadow {
		t.Errorf("no map: %s, want the deployment's shadow", got)
	}
}

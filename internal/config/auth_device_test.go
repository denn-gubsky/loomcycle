package config

import (
	"strings"
	"testing"
	"time"
)

func deviceGateConfig(mode string, g AuthGateConfig) *Config {
	c := passwordGateConfig(mode, AuthGateConfig{})
	g.Kind = AuthGateKindDevice
	c.Auth.Gates["cli"] = g
	return c
}

func TestResolveAuth_DeviceGateDefaultsAndIsNoLoginOption(t *testing.T) {
	c := deviceGateConfig("", AuthGateConfig{})
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	dg, ok := c.ResolvedAuth.DeviceGate()
	if !ok || dg.Device.TokenTTL != 12*time.Hour || dg.Device.RatePerMinute != 0 {
		t.Fatalf("device gate = %+v %v", dg, ok)
	}
	for _, g := range c.ResolvedAuth.LoginGates() {
		if g.Kind == AuthGateKindDevice {
			t.Fatal("the device gate is offered on the login page")
		}
	}
	c = deviceGateConfig("", AuthGateConfig{AuthGateDeviceConfig: AuthGateDeviceConfig{TokenTTL: "2h", Rate: &AuthGateRateConfig{PerMinute: 30, Burst: 5}}})
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	if dg, _ := c.ResolvedAuth.DeviceGate(); dg.Device.TokenTTL != 2*time.Hour || dg.Device.RatePerMinute != 30 || dg.Device.RateBurst != 5 {
		t.Fatalf("configured device gate = %+v", dg.Device)
	}
}

// Every key that would read as protection the gate cannot give fails the load.
func TestResolveAuth_DeviceGateRefusesWhatItCannotHonour(t *testing.T) {
	noURL := deviceGateConfig("", AuthGateConfig{})
	noURL.Env.PublicURL = ""
	noKey := deviceGateConfig("", AuthGateConfig{})
	noKey.Env.SecretKey = ""
	delete(noKey.Auth.Gates, "local") // the password gate needs the key itself
	withMap := deviceGateConfig("", AuthGateConfig{IdentityMap: &AuthIdentityMapConfig{Tenant: "acme"}})
	badRate := deviceGateConfig("", AuthGateConfig{AuthGateDeviceConfig: AuthGateDeviceConfig{Rate: &AuthGateRateConfig{PerMinute: 0}}})
	two := deviceGateConfig("", AuthGateConfig{})
	two.Auth.Gates["cli2"] = AuthGateConfig{Kind: AuthGateKindDevice}
	keysElsewhere := passwordGateConfig("", AuthGateConfig{AuthGateDeviceConfig: AuthGateDeviceConfig{TokenTTL: "1h"}})
	policyOnDevice := deviceGateConfig("", AuthGateConfig{})
	policyOnDevice.Auth.TenantPolicies = map[string]AuthTenantPolicyConfig{"acme": {Gates: []string{"cli"}}}
	strictOnlyDevice := deviceGateConfig(AuthModeStrict, AuthGateConfig{})
	delete(strictOnlyDevice.Auth.Gates, "local")
	for name, tc := range map[string]struct {
		c    *Config
		want string
	}{
		"no public URL":                     {noURL, "LOOMCYCLE_PUBLIC_URL"},
		"no pepper":                         {noKey, "LOOMCYCLE_SECRET_KEY"},
		"identity_map":                      {withMap, "identity_map"},
		"bad rate":                          {badRate, "rate"},
		"two device gates":                  {two, "at most one"},
		"device keys on a password gate":    {keysElsewhere, "only to kind: device"},
		"a tenant policy naming the device": {policyOnDevice, "tenant_policies"},
		"strict with only a device gate":    {strictOnlyDevice, "interactive gate"},
	} {
		err := resolveAuth(tc.c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

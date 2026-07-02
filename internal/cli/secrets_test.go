package cli

import (
	"flag"
	"strings"
	"testing"
)

// TestRegisterTokenFlag_DoesNotLeakDefaultInUsage is the regression for the
// --token flag leaking $LOOMCYCLE_AUTH_TOKEN: the flag's default value became
// its displayed DefValue, so flag.PrintDefaults (on -h or a parse error)
// printed the real admin bearer. The flag must still default to the env value
// (runtime behaviour) while its DISPLAYED default is blanked.
func TestRegisterTokenFlag_DoesNotLeakDefaultInUsage(t *testing.T) {
	t.Setenv("LOOMCYCLE_AUTH_TOKEN", "lct_supersecret_admin")

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	p := registerTokenFlag(fs)

	// Runtime behaviour preserved: an unparsed flag resolves to the env token.
	if *p != "lct_supersecret_admin" {
		t.Errorf("flag value = %q, want the env token (env fallback must still work)", *p)
	}
	// The DISPLAYED default is blanked so PrintDefaults can't leak it.
	f := fs.Lookup("token")
	if f == nil {
		t.Fatal("token flag not registered")
	}
	if f.DefValue != "" {
		t.Errorf("token flag DefValue = %q, want \"\" (the secret must not appear in usage)", f.DefValue)
	}
	// Belt-and-braces: the rendered usage must not contain the secret.
	var sb strings.Builder
	fs.SetOutput(&sb)
	fs.PrintDefaults()
	if strings.Contains(sb.String(), "lct_supersecret_admin") {
		t.Errorf("PrintDefaults leaked the token:\n%s", sb.String())
	}
}

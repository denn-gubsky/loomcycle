package helper_test

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/helper"
)

// TestCheckEnvName_RefusesEveryNameConfigCallsSecret is a drift guard: the
// runner keeps its own copy of the secret-name suffixes (to stay an import
// leaf), so every name config.IsSecretEnvName classifies as a secret must be
// refused here too. Probed over each suffix the runner knows, in both cases,
// and over a sample of names that are not secrets.
func TestCheckEnvName_RefusesEveryNameConfigCallsSecret(t *testing.T) {
	probes := []string{"TESSDATA_PREFIX", "OMP_NUM_THREADS", "LANG", "KEYBOARD", "TOKENIZER_MODE", "AUTHOR"}
	for _, s := range helper.SecretSuffixes {
		probes = append(probes, "SOME"+s, "some"+strings.ToLower(s))
	}
	for _, s := range []string{"_KEYS", "_PASS", "_PWD", "_APIKEY", "_PRIVATE_KEY", "_ACCESS_TOKEN", "_CERT"} {
		probes = append(probes, "SOME"+s)
	}
	for _, n := range probes {
		if config.IsSecretEnvName(n) && helper.CheckEnvName(n) == nil {
			t.Errorf("%s: config classifies it as a secret, the helper runner would pass it", n)
		}
	}
}

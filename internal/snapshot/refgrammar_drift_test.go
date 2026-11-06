package snapshot

import (
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// The snapshot scanners keep their own copies of the stored-credential
// reference grammar (they need the syntax as a group). They must find exactly
// the references the credential package resolves, or a reference would be
// reported as a literal secret, or missed by the missing-credential scan.
func TestRefGrammar_SnapshotRegexesMatchCredentialGrammar(t *testing.T) {
	long := strings.Repeat("n", 128)
	corpus := []string{
		"$cred:a", "$ghapp:b", "$oauth:c", "Bearer $oauth:c and $cred:a",
		"$oauth:" + long, "$oauth:" + long + "x", "$oauthx:c", "$OAUTH:c", "$oauth:", "$cred:a-b_C9",
	}
	for _, s := range corpus {
		want := credential.AnyRefNames(s)
		var got []string
		for _, m := range credRefRe.FindAllStringSubmatch(s, -1) {
			got = append(got, m[2])
		}
		// AnyRefNames groups by syntax; compare as sets of names.
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			t.Errorf("%q: credRefRe found %v, credential grammar %v", s, got, want)
		}
		var viaRef []string
		for _, m := range referenceRe.FindAllString(s, -1) {
			if !strings.HasPrefix(m, "${") {
				viaRef = append(viaRef, m[strings.Index(m, ":")+1:])
			}
		}
		slices.Sort(viaRef)
		if !slices.Equal(want, viaRef) {
			t.Errorf("%q: referenceRe found %v, credential grammar %v", s, viaRef, want)
		}
	}
}

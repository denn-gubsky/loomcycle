package config

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// A webhook or a schedule names the bearer of the run it starts as a stored
// credential, "$cred:<name>", never the secret itself: the reference rides
// into the run unopened, as a $cred: value in user_credentials does, and is
// resolved for each MCP request it is sent on, for that server's URL — so a
// credential restricted by allowed_urls works, and its value is in the run
// only for the request that sends it.

// CheckTriggerUserBearer checks a run-starting definition's user_bearer when
// it is written: empty, or one stored credential reference.
func CheckTriggerUserBearer(v string) error {
	if v == "" {
		return nil
	}
	if _, ok := credential.ParseRef(v); !ok {
		return fmt.Errorf("user_bearer takes only a stored credential, written \"$cred:<name>\": a literal bearer would be kept in the definition, readable at rest")
	}
	return nil
}

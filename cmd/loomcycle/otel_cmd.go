package main

import (
	"fmt"
	"io"
	"os"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

const otelUsage = "usage: loomcycle otel user-hash <tenant> <user>"

// runOtel is `loomcycle otel …`. Its one verb, user-hash, prints the
// loomcycle.user_hash a user's spans carry, so an investigation can find one
// user's traces without the backend holding the raw id. It needs the
// deployment's LOOMCYCLE_SECRET_KEY, and nothing else: no config, no store.
func runOtel(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[0] != "user-hash" {
		fmt.Fprintln(stderr, "loomcycle: error: "+otelUsage)
		return 2
	}
	key, err := credential.DeriveOTelUserKey(os.Getenv("LOOMCYCLE_SECRET_KEY"))
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: LOOMCYCLE_SECRET_KEY: %v\n", err)
		return 2
	}
	if key == nil {
		fmt.Fprintln(stderr, "loomcycle: error: LOOMCYCLE_SECRET_KEY is not set; the hash is keyed by the deployment's secret key (without one, spans carry the raw user id)")
		return 2
	}
	fmt.Fprintln(stdout, lcotel.UserHash(key, args[1], args[2]))
	return 0
}

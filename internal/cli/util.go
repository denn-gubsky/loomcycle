package cli

import "os"

// getenvDefault returns os.Getenv(name) if non-empty, otherwise fallback.
// Internal helper shared by health.go and migrate.go to avoid pulling
// the same trivial wrapper in from two places.
func getenvDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

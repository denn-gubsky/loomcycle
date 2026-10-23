// Package retentionmode is the vocabulary of a run's retention mode: whether its
// content is stored at all.
//
// It is apart from internal/store because the mode is written in definitions
// the config layer validates (an agent's `retention:`), and config sits below the
// store. The store persists the value; this package says what the values are.
package retentionmode

import "fmt"

// None is the retention mode of a run whose content is never stored.
// The default mode is the empty string.
const None = "none"

// Parse reads a retention mode as a caller wrote it: "none", or the
// default spelled "default" or left empty. It returns the stored form
// (None or ""). Anything else is an error — a misspelled "none" must
// not become a run that stores everything.
func Parse(s string) (string, error) {
	switch s {
	case "", "default":
		return "", nil
	case None:
		return None, nil
	}
	return "", fmt.Errorf("retention must be %q or %q, not %q", None, "default", s)
}

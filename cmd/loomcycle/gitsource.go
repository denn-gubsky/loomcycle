package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Limits of a volume created from a git source, and of the Git tool's
// fetches. Fixed: the host list is the operator's decision; these only keep
// one clone from exhausting the host. The object limit is what bounds memory:
// a pack is compressed, and the library holds each object whole while it
// indexes one, so the bytes fetched do not.
const (
	gitSourceMaxBytes       = 256 << 20 // fetched, and written, per clone
	gitSourceMaxObjectBytes = 64 << 20  // one object, inflated
	gitSourceMaxFileBytes   = 5 << 20   // one file written; a larger one is left out and listed
	gitSourceMaxFiles       = 100_000   // files, links and submodule directories per clone
	gitSourceTimeout        = 2 * time.Minute
	gitSourceMaxConcurrent  = 1 // clones at once per replica: each may hold a few objects in memory
	gitSourceMaxHistory     = 500
)

// gitSourceConfigFromEnv reads the operator's settings for git sources. It
// returns nil when LOOMCYCLE_GIT_SOURCE_HOSTS is unset: the feature is off
// and a request for it is refused.
func gitSourceConfigFromEnv(getenv func(string) string, privateHostAllowlist []string) (*builtin.GitSourceConfig, error) {
	var hosts []string
	for _, h := range strings.Split(getenv("LOOMCYCLE_GIT_SOURCE_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			if strings.ContainsAny(h, "/@ ") || strings.Contains(h, "://") {
				return nil, fmt.Errorf("LOOMCYCLE_GIT_SOURCE_HOSTS: %q is not a host name (write github.com, not a URL)", h)
			}
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	cfg := &builtin.GitSourceConfig{
		Hosts:          hosts,
		MaxBytes:       gitSourceMaxBytes,
		MaxObjectBytes: gitSourceMaxObjectBytes,
		MaxFileBytes:   gitSourceMaxFileBytes,
		MaxFiles:       gitSourceMaxFiles,
		Timeout:        gitSourceTimeout,
		Slots:          make(chan struct{}, gitSourceMaxConcurrent),
		MaxHistory:     gitSourceMaxHistory,
	}
	// Every dial goes through the guard: a host that resolves to a private
	// or metadata address is refused, unless the operator vouched for it.
	cfg.Transport = &http.Transport{
		DialContext:           netguard.GuardedDialContext(false, privateHostAllowlist),
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return cfg, nil
}

package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Defaults for a volume created from a git source. The object limit is what
// bounds memory: a pack is compressed, and the library holds each object
// whole while it indexes one, so the bytes fetched do not.
const (
	gitSourceDefaultMaxBytes       = 256 << 20
	gitSourceDefaultMaxObjectBytes = 64 << 20
	gitSourceDefaultMaxFileBytes   = 5 << 20
	gitSourceDefaultMaxFiles       = 100_000
	gitSourceDefaultTimeoutSeconds = 120
	gitSourceDefaultMaxConcurrent  = 1
)

// gitSourceConfigFromEnv reads the operator's settings for git sources. It
// returns nil when LOOMCYCLE_GIT_SOURCE_HOSTS is unset: the feature is off
// and a request for it is refused. A setting that is not a positive number is
// an error, not a silent default: an operator who wrote a limit meant one.
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
	num := func(name string, dflt int64) (int64, error) {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return dflt, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s=%q is not a positive number", name, v)
		}
		return n, nil
	}
	var (
		cfg = &builtin.GitSourceConfig{Hosts: hosts}
		err error
	)
	if cfg.MaxBytes, err = num("LOOMCYCLE_GIT_SOURCE_MAX_BYTES", gitSourceDefaultMaxBytes); err != nil {
		return nil, err
	}
	if cfg.MaxObjectBytes, err = num("LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES", gitSourceDefaultMaxObjectBytes); err != nil {
		return nil, err
	}
	if cfg.MaxFileBytes, err = num("LOOMCYCLE_GIT_SOURCE_MAX_FILE_BYTES", gitSourceDefaultMaxFileBytes); err != nil {
		return nil, err
	}
	files, err := num("LOOMCYCLE_GIT_SOURCE_MAX_FILES", gitSourceDefaultMaxFiles)
	if err != nil {
		return nil, err
	}
	cfg.MaxFiles = int(files)
	secs, err := num("LOOMCYCLE_GIT_SOURCE_TIMEOUT_SECONDS", gitSourceDefaultTimeoutSeconds)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = time.Duration(secs) * time.Second
	slots, err := num("LOOMCYCLE_GIT_SOURCE_MAX_CONCURRENT", gitSourceDefaultMaxConcurrent)
	if err != nil {
		return nil, err
	}
	cfg.Slots = make(chan struct{}, slots)
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

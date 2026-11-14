package config

import (
	"fmt"
	"net"
	"reflect"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Conversion modes (RFC GO §5.7).
const (
	// ConversionInProcess converts files in the runtime's own process, as it
	// always has. The default.
	ConversionInProcess = "inprocess"
	// ConversionLocal starts conversion workers as children of the runtime
	// and sends extraction to them, so a file that exhausts memory kills a
	// worker rather than the runtime.
	ConversionLocal = "local"
	// ConversionRemote listens for workers that run as their own containers
	// on conversion.listen, and sends every conversion to them.
	ConversionRemote = "remote"
	// ConversionLocalRemote is both: children of the runtime and workers of
	// their own, in one pool.
	ConversionLocalRemote = "local+remote"
)

// ConversionConfig is the `conversion:` block: where file conversions run
// (RFC GO). Policy and tuning, so yaml only (RFC GN). Resolved fills its
// defaults.
type ConversionConfig struct {
	// Mode is inprocess (default), local, remote or local+remote.
	Mode string `yaml:"mode,omitempty"`
	// Listen is the host:port the worker endpoint listens on for workers
	// of their own (modes remote and local+remote). Never the public port.
	Listen string `yaml:"listen,omitempty"`
	// TLS is the endpoint's certificate, and optionally the CA its workers'
	// client certificates must be signed by. Required for a listen address
	// off this host's loopback unless Insecure is set.
	TLS ConversionTLSConfig `yaml:"tls,omitempty"`
	// Insecure serves a listen address off the loopback without TLS: only
	// for a network the runtime and its workers share and nothing else can
	// read (a container-internal network, a WireGuard link).
	Insecure bool `yaml:"insecure,omitempty"`
	// QueueWaitS is how long a conversion waits for a free worker before the
	// call is answered as a busy server. Default 60.
	QueueWaitS int `yaml:"queue_wait_s,omitempty"`
	// QuarantineTTLS is how long a file that crashed a worker is refused.
	// Default 3600.
	QuarantineTTLS int `yaml:"quarantine_ttl_s,omitempty"`
	// Local tunes mode local.
	Local ConversionLocalConfig `yaml:"local,omitempty"`
}

// ConversionTLSConfig is `conversion.tls`.
type ConversionTLSConfig struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
	// ClientCAFile, when set, makes every worker present a client
	// certificate signed by it (mutual TLS), on top of the token.
	ClientCAFile string `yaml:"client_ca_file,omitempty"`
}

// Enabled reports whether a certificate is configured.
func (t ConversionTLSConfig) Enabled() bool { return t.CertFile != "" || t.KeyFile != "" }

// ConversionLocalConfig is `conversion.local`.
type ConversionLocalConfig struct {
	// Workers is the children kept running. Default 2.
	Workers int `yaml:"workers,omitempty"`
	// MemoryLimitMB is each child's soft memory limit (GOMEMLIMIT). Default
	// 1024.
	MemoryLimitMB int `yaml:"memory_limit_mb,omitempty"`
}

// Defaults and bounds of the conversion block.
const (
	DefaultConversionQueueWaitS     = 60
	DefaultConversionQuarantineTTLS = 3600
	DefaultConversionLocalWorkers   = 2
	DefaultConversionLocalMemoryMB  = 1024
	maxConversionLocalWorkers       = 64
	minConversionLocalMemoryMB      = 128
)

// Resolved is the block with its defaults filled in.
func (c ConversionConfig) Resolved() ConversionConfig {
	if c.Mode == "" {
		c.Mode = ConversionInProcess
	}
	if c.QueueWaitS == 0 {
		c.QueueWaitS = DefaultConversionQueueWaitS
	}
	if c.QuarantineTTLS == 0 {
		c.QuarantineTTLS = DefaultConversionQuarantineTTLS
	}
	if c.Local.Workers == 0 {
		c.Local.Workers = DefaultConversionLocalWorkers
	}
	if c.Local.MemoryLimitMB == 0 {
		c.Local.MemoryLimitMB = DefaultConversionLocalMemoryMB
	}
	return c
}

// LocalWorkers reports whether the mode starts workers as children of the runtime.
func (c ConversionConfig) LocalWorkers() bool {
	return c.Mode == ConversionLocal || c.Mode == ConversionLocalRemote
}

// RemoteWorkers reports whether the mode listens for workers of their own.
func (c ConversionConfig) RemoteWorkers() bool {
	return c.Mode == ConversionRemote || c.Mode == ConversionLocalRemote
}

// validateConversion refuses a mode it does not know, a remote mode with no
// worker endpoint or one on the runtime's own ports, and values out of range.
// Part of validate.
func validateConversion(c *Config) error {
	cv := c.Conversion
	switch cv.Mode {
	case "", ConversionInProcess, ConversionLocal, ConversionRemote, ConversionLocalRemote:
	default:
		return fmt.Errorf("conversion.mode: unknown mode %q (want %s, %s, %s or %s)", cv.Mode,
			ConversionInProcess, ConversionLocal, ConversionRemote, ConversionLocalRemote)
	}
	if cv.RemoteWorkers() {
		if cv.Listen == "" {
			return fmt.Errorf("conversion.listen: mode %s needs the host:port workers attach to", cv.Mode)
		}
		host, port, err := net.SplitHostPort(cv.Listen)
		if n, perr := strconv.Atoi(port); err != nil || perr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("conversion.listen: %q is not a host:port", cv.Listen)
		}
		for _, own := range []struct{ name, addr string }{
			{"LOOMCYCLE_LISTEN_ADDR", c.Env.ListenAddr}, {"LOOMCYCLE_GRPC_ADDR", c.Env.GrpcAddr}, {"the metrics listener", c.Env.MetricsListenAddr},
		} {
			if own.addr == "" {
				continue
			}
			if _, p, err := net.SplitHostPort(own.addr); err == nil && p == port && (host == "" || sameHost(host, own.addr)) {
				return fmt.Errorf("conversion.listen: %s is the port of %s; the worker endpoint is never a public port", cv.Listen, own.name)
			}
		}
		if cv.TLS.Enabled() && (cv.TLS.CertFile == "" || cv.TLS.KeyFile == "") {
			return fmt.Errorf("conversion.tls: cert_file and key_file are set together")
		}
		if cv.TLS.ClientCAFile != "" && !cv.TLS.Enabled() {
			return fmt.Errorf("conversion.tls.client_ca_file needs cert_file and key_file")
		}
		if !cv.TLS.Enabled() && !cv.Insecure && !loopbackHost(host) {
			return fmt.Errorf("conversion.listen: %s is reachable off this host, and the endpoint carries every file sent for conversion: set conversion.tls (cert_file, key_file), or conversion.insecure: true on a network only the runtime and its workers share", cv.Listen)
		}
	} else if cv.Listen != "" || cv.TLS.Enabled() || cv.TLS.ClientCAFile != "" || cv.Insecure {
		return fmt.Errorf("conversion.listen, tls and insecure: set only in mode %s or %s", ConversionRemote, ConversionLocalRemote)
	}
	if cv.QueueWaitS < 0 || cv.QuarantineTTLS < 0 {
		return fmt.Errorf("conversion: queue_wait_s and quarantine_ttl_s may not be negative")
	}
	if w := cv.Local.Workers; w < 0 || w > maxConversionLocalWorkers {
		return fmt.Errorf("conversion.local.workers: %d is outside 1..%d", w, maxConversionLocalWorkers)
	}
	if m := cv.Local.MemoryLimitMB; m != 0 && m < minConversionLocalMemoryMB {
		return fmt.Errorf("conversion.local.memory_limit_mb: %d is under the %d MiB a worker needs", m, minConversionLocalMemoryMB)
	}
	return nil
}

// loopbackHost reports whether host is this machine's loopback only.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sameHost reports whether host is addr's host or either listens on every
// interface, so the two could collide on one port.
func sameHost(host, addr string) bool {
	h, _, _ := net.SplitHostPort(addr)
	wild := func(x string) bool { return x == "" || x == "0.0.0.0" || x == "::" }
	return wild(host) || wild(h) || h == host
}

func (c *ConversionConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "conversion", nil, reflect.TypeOf(*c)); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "local"), "conversion.local", nil, reflect.TypeOf(ConversionLocalConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "tls"), "conversion.tls", nil, reflect.TypeOf(ConversionTLSConfig{})); err != nil {
		return err
	}
	type plain ConversionConfig
	return n.Decode((*plain)(c))
}

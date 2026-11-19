package config

import (
	"fmt"
	"net"
	"reflect"
	"strconv"

	"gopkg.in/yaml.v3"
)

// WorkerEndpointConfig is the `worker_endpoint:` block: the address workers
// of their own attach to, and its TLS (RFC GW Q1). It is neutral of what the
// workers do, so a later worker role can use the endpoint whatever
// conversion.mode is. conversion.listen, conversion.tls and
// conversion.insecure are the same settings written the older way; they are
// still read (Config.ResolvedWorkerEndpoint).
type WorkerEndpointConfig struct {
	// Listen is the host:port the endpoint listens on. Never a public port.
	Listen string `yaml:"listen,omitempty"`
	// TLS is the endpoint's certificate, and optionally the CA its workers'
	// client certificates must be signed by. Required for a listen address
	// off this host's loopback unless Insecure is set.
	TLS WorkerEndpointTLSConfig `yaml:"tls,omitempty"`
	// Insecure serves a listen address off the loopback without TLS: only
	// for a network the runtime and its workers share and nothing else can
	// read (a container-internal network, a WireGuard link).
	Insecure bool `yaml:"insecure,omitempty"`
}

// WorkerEndpointTLSConfig is `worker_endpoint.tls` (and `conversion.tls`).
type WorkerEndpointTLSConfig struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
	// ClientCAFile, when set, makes every worker present a client
	// certificate signed by it (mutual TLS), on top of its bearer.
	ClientCAFile string `yaml:"client_ca_file,omitempty"`
}

// Enabled reports whether a certificate is configured.
func (t WorkerEndpointTLSConfig) Enabled() bool { return t.CertFile != "" || t.KeyFile != "" }

// set reports whether the block writes any value.
func (e WorkerEndpointConfig) set() bool {
	return e.Listen != "" || e.TLS != (WorkerEndpointTLSConfig{}) || e.Insecure
}

// ResolvedWorkerEndpoint is the worker endpoint to serve: each value of the
// worker_endpoint block, else the same value of the conversion block. Load
// has refused a config whose two blocks set one value differently.
func (c *Config) ResolvedWorkerEndpoint() WorkerEndpointConfig {
	ep, _, _ := resolveWorkerEndpoint(c)
	return ep
}

// resolveWorkerEndpoint merges the two blocks value by value. key is the
// block the settings are reported under: worker_endpoint when it writes
// anything, else conversion, so a config written the older way keeps the
// messages it had.
func resolveWorkerEndpoint(c *Config) (ep WorkerEndpointConfig, key string, err error) {
	ne, old := c.WorkerEndpoint, c.Conversion
	pick := func(name, n, o string) string {
		if n != "" && o != "" && n != o && err == nil {
			err = fmt.Errorf("worker_endpoint.%s (%q) and conversion.%s (%q) are both set and differ: set worker_endpoint.%s only; conversion.%s is its older name", name, n, name, o, name, name)
		}
		if n != "" {
			return n
		}
		return o
	}
	ep.Listen = pick("listen", ne.Listen, old.Listen)
	ep.TLS.CertFile = pick("tls.cert_file", ne.TLS.CertFile, old.TLS.CertFile)
	ep.TLS.KeyFile = pick("tls.key_file", ne.TLS.KeyFile, old.TLS.KeyFile)
	ep.TLS.ClientCAFile = pick("tls.client_ca_file", ne.TLS.ClientCAFile, old.TLS.ClientCAFile)
	// A bool cannot be told unset from false, so true in either block is
	// true: the two never differ in a way that could be refused.
	ep.Insecure = ne.Insecure || old.Insecure
	key = "conversion"
	if ne.set() {
		key = "worker_endpoint"
	}
	return ep, key, err
}

// validateWorkerEndpoint refuses an endpoint that is not a host:port, that
// is on one of the runtime's own ports, whose TLS is half written, or that
// is reachable off this host in the clear without the operator saying the
// network is private. key names the block in messages.
func validateWorkerEndpoint(c *Config, ep WorkerEndpointConfig, key string) error {
	host, port, err := net.SplitHostPort(ep.Listen)
	if n, perr := strconv.Atoi(port); err != nil || perr != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s.listen: %q is not a host:port", key, ep.Listen)
	}
	for _, own := range []struct{ name, addr string }{
		{"LOOMCYCLE_LISTEN_ADDR", c.Env.ListenAddr}, {"LOOMCYCLE_GRPC_ADDR", c.Env.GrpcAddr}, {"the metrics listener", c.Env.MetricsListenAddr},
	} {
		if own.addr == "" {
			continue
		}
		if _, p, err := net.SplitHostPort(own.addr); err == nil && p == port && (host == "" || sameHost(host, own.addr)) {
			return fmt.Errorf("%s.listen: %s is the port of %s; the worker endpoint is never a public port", key, ep.Listen, own.name)
		}
	}
	if ep.TLS.Enabled() && (ep.TLS.CertFile == "" || ep.TLS.KeyFile == "") {
		return fmt.Errorf("%s.tls: cert_file and key_file are set together", key)
	}
	if ep.TLS.ClientCAFile != "" && !ep.TLS.Enabled() {
		return fmt.Errorf("%s.tls.client_ca_file needs cert_file and key_file", key)
	}
	if !ep.TLS.Enabled() && !ep.Insecure && !loopbackHost(host) {
		return fmt.Errorf("%[1]s.listen: %[2]s is reachable off this host, and the endpoint carries every file sent to a worker: set %[1]s.tls (cert_file, key_file), or %[1]s.insecure: true on a network only the runtime and its workers share", key, ep.Listen)
	}
	return nil
}

func (e *WorkerEndpointConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "worker_endpoint", nil, reflect.TypeOf(*e)); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "tls"), "worker_endpoint.tls", nil, reflect.TypeOf(WorkerEndpointTLSConfig{})); err != nil {
		return err
	}
	type plain WorkerEndpointConfig
	return n.Decode((*plain)(e))
}

package config

import (
	"fmt"
	"net"
	"reflect"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Conversion modes (RFC GO §5.7).
const (
	// ConversionInProcess converts files in the runtime's own process, as it
	// always has. The default off Linux.
	ConversionInProcess = "inprocess"
	// ConversionLocal starts conversion workers as children of the runtime
	// and sends every conversion to them, so a file that exhausts memory
	// kills a worker rather than the runtime. The default on Linux (RFC GO
	// D2), where the children's OOM score makes the kernel pick them first.
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
	// Listen, TLS and Insecure are worker_endpoint's listen, tls and
	// insecure written the older way, read only in modes remote and
	// local+remote. A value worker_endpoint leaves unset is taken from here;
	// one both blocks set must agree (Config.ResolvedWorkerEndpoint).
	Listen   string                  `yaml:"listen,omitempty"`
	TLS      WorkerEndpointTLSConfig `yaml:"tls,omitempty"`
	Insecure bool                    `yaml:"insecure,omitempty"`
	// QueueWaitS is how long a conversion waits for a free worker before the
	// call is answered as a busy server. Default 60.
	QueueWaitS int `yaml:"queue_wait_s,omitempty"`
	// QuarantineTTLS is how long a file that crashed a worker is refused.
	// Default 3600.
	QuarantineTTLS int `yaml:"quarantine_ttl_s,omitempty"`
	// Local tunes mode local.
	Local ConversionLocalConfig `yaml:"local,omitempty"`
}

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

// DefaultConversionMode is the mode a config that writes none runs in on
// goos: local on Linux, where a child's raised OOM score makes the kernel
// kill a worker before the runtime; inprocess elsewhere, where nothing
// steers the kernel's choice and a child would only add processes.
func DefaultConversionMode(goos string) string {
	if goos == "linux" {
		return ConversionLocal
	}
	return ConversionInProcess
}

// Resolved is the block with its defaults filled in.
func (c ConversionConfig) Resolved() ConversionConfig {
	if c.Mode == "" {
		c.Mode = DefaultConversionMode(runtime.GOOS)
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

// validateConversion refuses a mode it does not know, worker_endpoint and the
// older conversion keys setting one value differently, a remote mode with no
// worker endpoint or a bad one (validateWorkerEndpoint), and values out of
// range.
// Part of validate.
func validateConversion(c *Config) error {
	cv := c.Conversion
	switch cv.Mode {
	case "", ConversionInProcess, ConversionLocal, ConversionRemote, ConversionLocalRemote:
	default:
		return fmt.Errorf("conversion.mode: unknown mode %q (want %s, %s, %s or %s)", cv.Mode,
			ConversionInProcess, ConversionLocal, ConversionRemote, ConversionLocalRemote)
	}
	ep, key, err := resolveWorkerEndpoint(c)
	if err != nil {
		return err
	}
	switch {
	case cv.RemoteWorkers():
		if ep.Listen == "" {
			return fmt.Errorf("worker_endpoint.listen: conversion mode %s needs the host:port workers attach to", cv.Mode)
		}
		if err := validateWorkerEndpoint(c, ep, key); err != nil {
			return err
		}
	case cv.Listen != "" || cv.TLS != (WorkerEndpointTLSConfig{}) || cv.Insecure:
		return fmt.Errorf("conversion.listen, tls and insecure: set only in mode %s or %s", ConversionRemote, ConversionLocalRemote)
	case c.WorkerEndpoint.set():
		// Conversion attaches no workers of its own in this mode; workspace
		// workers (RFC GW) may. The values are checked either way, so a
		// mistake does not wait for the day they are used.
		if ep.Listen == "" {
			return fmt.Errorf("worker_endpoint.listen: the host:port workers attach to is required when the block is written")
		}
		if err := validateWorkerEndpoint(c, ep, key); err != nil {
			return err
		}
		if !c.Workspace.RemoteWorkers() {
			c.Warnings = append(c.Warnings, fmt.Sprintf("worker_endpoint is set, but neither conversion.mode %s nor workspace.mode attaches workers of their own: nothing listens on %s", cv.Mode, ep.Listen))
		}
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
	if err := strictKeys(valueOf(n, "tls"), "conversion.tls", nil, reflect.TypeOf(WorkerEndpointTLSConfig{})); err != nil {
		return err
	}
	type plain ConversionConfig
	return n.Decode((*plain)(c))
}

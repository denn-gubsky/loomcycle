package config

import (
	"fmt"
	"reflect"

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
)

// conversionModesNotYet are the modes RFC GO P3 adds.
var conversionModesNotYet = map[string]bool{"remote": true, "local+remote": true}

// ConversionConfig is the `conversion:` block: where file conversions run
// (RFC GO). Policy and tuning, so yaml only (RFC GN). Resolved fills its
// defaults.
type ConversionConfig struct {
	// Mode is inprocess (default) or local.
	Mode string `yaml:"mode,omitempty"`
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

// validateConversion refuses a mode it does not know or does not have yet,
// and values out of range. Part of validate.
func validateConversion(c *Config) error {
	cv := c.Conversion
	switch cv.Mode {
	case "", ConversionInProcess, ConversionLocal:
	default:
		if conversionModesNotYet[cv.Mode] {
			return fmt.Errorf("conversion.mode: %q is not available yet; use %s or %s", cv.Mode, ConversionInProcess, ConversionLocal)
		}
		return fmt.Errorf("conversion.mode: unknown mode %q (want %s or %s)", cv.Mode, ConversionInProcess, ConversionLocal)
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

func (c *ConversionConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "conversion", nil, reflect.TypeOf(*c)); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "local"), "conversion.local", nil, reflect.TypeOf(ConversionLocalConfig{})); err != nil {
		return err
	}
	type plain ConversionConfig
	return n.Decode((*plain)(c))
}

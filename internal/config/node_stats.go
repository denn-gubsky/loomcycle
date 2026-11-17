package config

import "time"

// NodeStatsConfig is the memory.node_stats block: the background pass that keeps
// the statistics of the tenant taxonomy's nodes (RFC GM P2.1) — member counts and,
// where the store keeps vectors, spread, exemplars, nearest siblings and
// distinctive terms. Document op=node_stats runs the same pass on demand whatever
// this block says.
//
// OFF BY DEFAULT, and off means no sweeper starts and no lock is taken. On, the
// sweeper needs a positive interval too. It makes no model call: it reads stored
// annotations and their vectors, and recomputes only the nodes whose inputs moved.
type NodeStatsConfig struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the sweeper walks every tenant scope with a tenant
	// ontology (a Go duration, e.g. "6h"). Empty or "0" starts no sweeper.
	Interval string `yaml:"interval"`
}

// NodeStatsSettings is NodeStatsConfig resolved. Built by Config.DocumentNodeStats.
type NodeStatsSettings struct {
	Enabled  bool
	Interval time.Duration
}

// resolve parses the interval. An invalid duration is an error here and a boot
// failure through validate.
func (n NodeStatsConfig) resolve() (NodeStatsSettings, error) {
	s := NodeStatsSettings{Enabled: n.Enabled}
	var err error
	s.Interval, err = parseProjectionDuration("memory.node_stats.interval", n.Interval)
	return s, err
}

// DocumentNodeStats is the resolved memory.node_stats block. A nil Config, or a
// value validate would refuse, resolves to off.
func (c *Config) DocumentNodeStats() NodeStatsSettings {
	if c == nil {
		return NodeStatsSettings{}
	}
	s, err := c.Memory.NodeStats.resolve()
	if err != nil {
		return NodeStatsSettings{}
	}
	return s
}

package config

// The blocks validate checks, in its order; see validate.

import (
	"fmt"

	"github.com/robfig/cron/v3"

	"github.com/denn-gubsky/loomcycle/internal/teamspec"
)

// validateChannels checks the top-level channels block. Part of validate.
func validateChannels(c *Config) error {
	// Channel tool: validate the top-level `channels:` block.
	for name, ch := range c.Channels {
		if name == "" {
			return fmt.Errorf("channels: empty channel name")
		}
		// A team's own channels are stored under this prefix; one declared
		// here would read and write a team's messages from outside it.
		if teamspec.IsChannelName(name) {
			return fmt.Errorf("channels.%s: the %q prefix is reserved for a team's own channels, which a team declares in its definition under local.channels", name, teamspec.ChannelPrefix)
		}
		if !validChannelScopes[ch.Scope] {
			return fmt.Errorf("channels.%s: unknown scope %q (want one of: agent, user, tenant, global)", name, ch.Scope)
		}
		if ch.DefaultTTL < 0 {
			return fmt.Errorf("channels.%s: default_ttl must be >= 0", name)
		}
		if ch.MaxMessages < 0 {
			return fmt.Errorf("channels.%s: max_messages must be >= 0", name)
		}
		if ch.Semantic != "" && !validChannelSemantics[ch.Semantic] {
			return fmt.Errorf("channels.%s: unknown semantic %q (want one of: queue, broadcast)", name, ch.Semantic)
		}
		// v0.8.6 system-channels validation.
		if ch.Publisher != "" && ch.Publisher != "system" {
			return fmt.Errorf("channels.%s: unknown publisher %q (want: \"\" or \"system\")", name, ch.Publisher)
		}
		periodDur, err := ch.PeriodDuration()
		if err != nil {
			return fmt.Errorf("channels.%s: invalid period %q: %w", name, ch.Period, err)
		}
		if ch.Period != "" && ch.Publisher != "system" {
			return fmt.Errorf("channels.%s: period is only valid on `publisher: system` channels", name)
		}
		if ch.Publisher == "system" && periodDur == 0 && !eventDrivenSystemChannels[name] {
			return fmt.Errorf("channels.%s: publisher: system requires a `period:` (cadence) or the channel name must be in the event-driven set (%v)", name, eventDrivenSystemChannelNames())
		}
		if err := ValidateChannelHooks(name, ch.Publisher, ch.Hooks); err != nil {
			return fmt.Errorf("channels.%s: %w", name, err)
		}
		// `_system/` prefix is reserved — channels with this prefix
		// can only be operator-declared (we're inside the iteration
		// over the operator yaml, so any channel here IS declared);
		// agents still cannot publish to them regardless of Publisher
		// (enforced at tool layer).
	}
	return nil
}

// validateMCPServers checks the static MCP servers. Part of validate.
func validateMCPServers(c *Config) error {
	for name, srv := range c.MCPServers {
		switch srv.Transport {
		case "stdio":
			if srv.Command == "" {
				return fmt.Errorf("mcp_servers.%s: stdio transport requires command", name)
			}
		case "http":
			if srv.URL == "" {
				return fmt.Errorf("mcp_servers.%s: http transport requires url", name)
			}
		default:
			return fmt.Errorf("mcp_servers.%s: unknown transport %q", name, srv.Transport)
		}
		if err := ValidateBinaryResults(srv.BinaryResults); err != nil {
			return fmt.Errorf("mcp_servers.%s: %w", name, err)
		}
	}
	return nil
}

// validateScheduledRuns checks the static scheduled runs. Part of validate.
func validateScheduledRuns(c *Config) error {
	// v1.x RFC E scheduled_runs validation. Cron syntax parses,
	// agent name resolves (statically — substrate-active checks are
	// deferred to runtime because the substrate isn't loaded at
	// config-load time), on_complete kinds in closed set, env-allowlist
	// for from_env references.
	for name, sr := range c.ScheduledRuns {
		if name == "" {
			return fmt.Errorf("scheduled_runs: empty schedule name")
		}
		if err := validateScheduleDelivery(name, sr); err != nil {
			return err
		}
		if sr.Agent != "" {
			if _, ok := c.Agents[sr.Agent]; !ok {
				return fmt.Errorf("scheduled_runs.%s: agent %q not declared in cfg.Agents (substrate-only agents are resolved at runtime; declare a yaml stub if you want compile-time validation)", name, sr.Agent)
			}
		}
		// Mutual exclusion: standalone-schedule vs template-with-tier-defaults.
		if sr.Schedule != "" && len(sr.UserTierSchedules) > 0 {
			return fmt.Errorf("scheduled_runs.%s: cannot set both schedule: and user_tier_schedules: (pick one — schedule for standalone, user_tier_schedules for template)", name)
		}
		// Validate cron expressions where present. robfig/cron/v3
		// ParseStandard is the canonical 5-field parser the sweeper
		// uses; we validate against the same.
		if sr.Schedule != "" {
			if _, err := cron.ParseStandard(sr.Schedule); err != nil {
				return fmt.Errorf("scheduled_runs.%s: invalid cron expression %q: %w", name, sr.Schedule, err)
			}
		}
		for tier, cronExpr := range sr.UserTierSchedules {
			if _, err := cron.ParseStandard(cronExpr); err != nil {
				return fmt.Errorf("scheduled_runs.%s: user_tier_schedules.%s: invalid cron expression %q: %w", name, tier, cronExpr, err)
			}
		}
		if sr.CatchUpMax < 0 || sr.CatchUpMax > MaxScheduleCatchUp {
			return fmt.Errorf("scheduled_runs.%s: catch_up_max must be between 0 and %d", name, MaxScheduleCatchUp)
		}
		if err := CheckScheduleConcurrencyPolicy(sr.ConcurrencyPolicy, sr.Delivery); err != nil {
			return fmt.Errorf("scheduled_runs.%s: %w", name, err)
		}
		// Validate on_complete kinds.
		for i, hook := range sr.OnComplete {
			switch hook.Kind {
			case "channel.publish":
				if hook.Channel == "" {
					return fmt.Errorf("scheduled_runs.%s.on_complete[%d]: channel required for channel.publish", name, i)
				}
			case "mcp.call":
				if hook.Server == "" || hook.Tool == "" {
					return fmt.Errorf("scheduled_runs.%s.on_complete[%d]: server + tool required for mcp.call", name, i)
				}
			case "memory.set":
				if hook.Scope == "" || hook.Key == "" {
					return fmt.Errorf("scheduled_runs.%s.on_complete[%d]: scope + key required for memory.set", name, i)
				}
			default:
				return fmt.Errorf("scheduled_runs.%s.on_complete[%d]: unknown kind %q (want: channel.publish | mcp.call | memory.set)", name, i, hook.Kind)
			}
		}
	}
	return nil
}

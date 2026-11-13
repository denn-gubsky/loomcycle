package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// movedSetting is a retired variable whose setting moved to a yaml key. One
// rule holds for all of them: the load is refused when the variable gave
// something other than the default and the yaml does not name the key, and
// the variable is only advised when it gave the default, or when the yaml
// names the key with any value. Naming the key is the operator saying what
// they want; a variable set to what the runtime does anyway loses nothing.
//
// It holds in both directions. A timeout raised for a slow model is lost as
// silently as one lowered, and the first sign would be calls failing.
type movedSetting struct {
	// key is the yaml key the setting moved to, as the refusal names it.
	key string
	// differs reports whether value, read the way the variable was read,
	// gave something other than the default this version applies. A value
	// the old read ignored gave the default.
	differs func(value string, c retiredEnvCtx) bool
	// named reports whether the yaml names the key. Nil asks the loaded
	// yaml for key itself.
	named func(c retiredEnvCtx) bool
	// live reports whether the variable had any effect before: one read only
	// while a switch was on changed nothing with the switch off, and its
	// loss changes nothing. Nil is always.
	live func(c retiredEnvCtx) bool
	// lacking, for a variable that set one option on several providers,
	// returns the ids of those that do not name it; it replaces named.
	lacking func(c retiredEnvCtx) []string
}

// refusal is the "what to do" of name's refusal entry, or false when the
// variable is only advised.
func (m movedSetting) refusal(name, value string, c retiredEnvCtx) (string, bool) {
	if (m.live != nil && !m.live(c)) || !m.differs(value, c) {
		return "", false
	}
	if m.lacking != nil {
		ids := m.lacking(c)
		if len(ids) == 0 {
			return "", false
		}
		return fmt.Sprintf("%s; the variable was set to something other than the default and the option is missing on these providers: %s (set it on each, with the value you want that provider to have, or remove the variable to take the default)", retiredEnv[name], strings.Join(ids, ", ")), true
	}
	named := c.names(m.key)
	if m.named != nil {
		named = m.named(c)
	}
	if named {
		return "", false
	}
	return fmt.Sprintf("%s; the variable was set to something other than the default and the yaml does not name %s (name the key, with the value you want, or remove the variable to take the default)", retiredEnv[name], m.key), true
}

// names reports whether the loaded yaml names key (a dotted path).
func (c retiredEnvCtx) names(key string) bool { return c.cfg.yamlKeys[key] }

// retiredEnvMoved holds the rule of every retired variable whose setting
// moved to a yaml key in the release that wrote the `retention:`,
// `timeout_scaling:`, `metrics:` and provider option keys. A variable is here
// or in retiredEnvRefuse, never both
// (TestRetiredEnvMoved_EveryMovedVariableHasOneRule).
var retiredEnvMoved = map[string]movedSetting{
	// Retention. A family variable was read by a sweeper that ran only for
	// LOOMCYCLE_RETENTION_ENABLED=1, so without that it changed nothing. The
	// switch itself has its own rule in retiredEnvRefuse.
	"LOOMCYCLE_RETENTION_INTERVAL_MS":              {key: "retention.interval", differs: positiveOtherThan(3_600_000), live: retentionRan},
	"LOOMCYCLE_RETENTION_EXPORT_DIR":               {key: "retention.export_dir", differs: valueNotEmpty, live: retentionRan},
	"LOOMCYCLE_RETENTION_DEFS_MODE":                {key: "retention.defs.mode", differs: modeDeleted, live: retentionRan},
	"LOOMCYCLE_RETENTION_DEFS_MAX_AGE_MS":          {key: "retention.defs.max_age", differs: positiveOtherThan(0), live: retentionRan},
	"LOOMCYCLE_RETENTION_CHATS_MODE":               {key: "retention.chats.mode", differs: modeDeleted, live: retentionRan},
	"LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS":         {key: "retention.chats.max_age", differs: positiveOtherThan(0), live: retentionRan},
	"LOOMCYCLE_RETENTION_MEM_MODE":                 {key: "retention.mem.mode", differs: modeDeleted, live: retentionRan},
	"LOOMCYCLE_RETENTION_MEM_MAX_AGE_MS":           {key: "retention.mem.max_age", differs: positiveOtherThan(0), live: retentionRan},
	"LOOMCYCLE_RETENTION_MEM_CONTENT_MODE":         {key: "retention.mem_content.mode", differs: modeDeleted, live: retentionRan},
	"LOOMCYCLE_RETENTION_MEM_CONTENT_MAX_AGE_MS":   {key: "retention.mem_content.max_age", differs: positiveOtherThan(0), live: retentionRan},
	"LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MODE":       {key: "retention.empty_dossier.mode", differs: modeDeleted, live: retentionRan},
	"LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MAX_AGE_MS": {key: "retention.empty_dossier.max_age", differs: positiveOtherThan(0), live: retentionRan},
	// The old read took any number from zero up; zero keeps no version.
	"LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N": {key: "retention.defs.keep_last_n", live: retentionRan, differs: func(value string, _ retiredEnvCtx) bool {
		n, set, ok := retiredInt(value)
		return set && ok && n >= 0 && n != retentionDefsKeepLastNDefault
	}},
	// The usage rollup always runs. The trace age was read only by the trace
	// index, which has its own switch.
	"LOOMCYCLE_USAGE_DETAIL_RETENTION_MS": {key: "retention.usage_detail.max_age", differs: positiveOtherThan(retentionAgeDefault.Milliseconds())},
	"LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS": {key: "retention.trace.max_age", differs: positiveOtherThan(retentionAgeDefault.Milliseconds()), live: func(c retiredEnvCtx) bool {
		// As applyEnvTraceIndex reads the switch.
		v := c.env["LOOMCYCLE_MEMORY_TRACE_INDEX"]
		return v == "1" || strings.EqualFold(v, "true")
	}},

	// Provider stream timeouts. The global pair applied to every provider
	// but ollama-local, which had its own.
	"LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS": {
		key: "providers.<id>.options.header_timeout_ms", differs: positiveOtherThan(ProviderHeaderTimeoutMS),
		lacking: func(c retiredEnvCtx) []string { return providersLacking(c, "header_timeout_ms") },
	},
	"LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS": {
		key: "providers.<id>.options.idle_timeout_ms", differs: positiveOtherThan(ProviderIdleTimeoutMS),
		lacking: func(c retiredEnvCtx) []string { return providersLacking(c, "idle_timeout_ms") },
	},
	"LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS": {
		key: "providers.ollama-local.options.header_timeout_ms", differs: positiveOtherThan(OllamaLocalStreamTimeoutMS),
		named: providerOptionApplied("ollama-local", "header_timeout_ms"), live: ollamaLocalUsable,
	},
	"LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS": {
		key: "providers.ollama-local.options.idle_timeout_ms", differs: positiveOtherThan(OllamaLocalStreamTimeoutMS),
		named: providerOptionApplied("ollama-local", "idle_timeout_ms"), live: ollamaLocalUsable,
	},
	// The code-agent options, read only by a declared code-js provider.
	"LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS": {
		key: "providers.code-js.options.run_timeout_seconds", differs: positiveOtherThan(120),
		named: providerOptionApplied("code-js", "run_timeout_seconds"), live: providerDeclared("code-js"),
	},
	"LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS": {
		key: "providers.code-js.options.max_wall_seconds", differs: positiveOtherThan(86_400),
		named: providerOptionApplied("code-js", "max_wall_seconds"), live: providerDeclared("code-js"),
	},
	"LOOMCYCLE_CODE_AGENTS_DETERMINISTIC": {
		key: "providers.code-js.options.deterministic", differs: wasExactlyOne, live: providerDeclared("code-js"),
		named: func(c retiredEnvCtx) bool {
			_, named := c.cfg.Providers["code-js"].Options["deterministic"]
			return named
		},
	},

	// Timeout scaling. The mode was lower-cased; measure is the default, so
	// off is a setting. A mode that is neither stopped the boot, and does.
	"LOOMCYCLE_TIMEOUT_SCALING": {key: "timeout_scaling.mode", differs: func(value string, _ retiredEnvCtx) bool {
		v := strings.ToLower(strings.TrimSpace(value))
		return v != "" && v != TimeoutScalingMeasure
	}},
	"LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS":  {key: "timeout_scaling.reference.decode_tps", differs: positiveFloatOtherThan(defaultScalingDecodeTPS)},
	"LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER": {key: "timeout_scaling.max_multiplier", differs: positiveFloatOtherThan(defaultScalingMaxMultiplier)},

	// The embedder. The old reads took any whole number: zero and below
	// meant one text per call, and no timeout.
	"LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE": {key: "memory.embedder.batch_size", differs: intOtherThan(EmbedderBatchSizeDefault)},
	"LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS": {key: "memory.embedder.timeout_ms", differs: intOtherThan(EmbedderTimeoutMSDefault)},
	// A positive interval started the projection sweeper and turned the
	// projection on, and one that did not parse stopped the boot. None, or
	// zero, starts no sweeper. The yaml says the same in two keys, and the
	// interval alone would name a cadence for a projection that is off, so
	// both must be named.
	"LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL": {
		key: "memory.projection.interval and memory.projection.enabled",
		differs: func(value string, _ retiredEnvCtx) bool {
			d, err := parseProjectionDuration("", value)
			return err != nil || d > 0
		},
		named: func(c retiredEnvCtx) bool {
			return c.names("memory.projection.interval") && c.names("memory.projection.enabled")
		},
	},

	// Concurrency. A per-user cap of zero or below is "no cap"; the queue
	// bound's zero is "refuse at once", and the old read ignored a negative
	// number; a provider queue depth of zero inherits max_queue_depth, as an
	// absent key does, so that depth is its default.
	"LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER": {key: "concurrency.max_concurrent_runs_per_user", differs: positiveOtherThan(0)},
	"LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER": {key: "concurrency.max_queued_runs_per_user", differs: func(value string, _ retiredEnvCtx) bool {
		n, set, ok := retiredInt(value)
		return set && ok && n >= 0 && n != maxQueuedRunsPerUserDefault
	}},
	"LOOMCYCLE_PROVIDER_QUEUE_DEPTH": {key: "concurrency.provider_queue_depth", differs: func(value string, c retiredEnvCtx) bool {
		return positiveOtherThan(int64(c.cfg.Concurrency.MaxQueueDepth))(value, c)
	}},

	// Lists and maps. Any entry was a setting: the default is none.
	"LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS": {key: "observability.tracing.tenant_sampler_ratios", differs: func(value string, _ retiredEnvCtx) bool {
		return len(splitCSV(value)) > 0
	}},
	// Read only while tenant telemetry is on. With no list a tenant's
	// collector endpoint may name any host, so a list lost is a restriction
	// lost.
	"LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS": {
		key:     "observability.tenant_telemetry.allowed_hosts",
		differs: func(value string, _ retiredEnvCtx) bool { return len(splitCSV(value)) > 0 },
		live: func(c retiredEnvCtx) bool {
			// As tenanttelemetry.ConfigFromEnv reads the switch.
			on, err := strconv.ParseBool(strings.TrimSpace(c.env["LOOMCYCLE_TENANT_TELEMETRY"]))
			return err == nil && on
		},
	},
	// An origin the yaml does not list can no longer hand the login page a
	// token: the landing page's sign-in would stop working.
	"LOOMCYCLE_UI_LOGIN_ORIGINS": {key: "auth.login_origins", differs: func(value string, _ retiredEnvCtx) bool {
		return len(splitCommaTrim(value)) > 0
	}},

	// The process metrics sampler. Its two tunings were read only by a
	// sampler that LOOMCYCLE_METRICS_ENABLED=1 had started.
	"LOOMCYCLE_METRICS_ENABLED":        {key: "metrics.enabled", differs: wasExactlyOne},
	"LOOMCYCLE_METRICS_COLLECT_SYSTEM": {key: "metrics.collect_system", differs: wasExactlyOne, live: metricsRan},
	"LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS": {key: "metrics.sample_interval", live: metricsRan, differs: func(value string, _ retiredEnvCtx) bool {
		// The old read raised anything under a second to one second.
		n, set, ok := retiredInt(value)
		return set && ok && n > 0 && max(n, metricsSampleIntervalMin.Milliseconds()) != metricsSampleIntervalDefault.Milliseconds()
	}},
}

// maxQueuedRunsPerUserDefault is concurrency.max_queued_runs_per_user when
// the yaml does not name it; loadLayerTree seeds it.
const maxQueuedRunsPerUserDefault = 16

func retentionRan(c retiredEnvCtx) bool { return c.env["LOOMCYCLE_RETENTION_ENABLED"] == "1" }

func metricsRan(c retiredEnvCtx) bool { return c.env["LOOMCYCLE_METRICS_ENABLED"] == "1" }

// wasExactlyOne is the differs of a default-off switch whose old read was
// `== "1"`.
func wasExactlyOne(value string, _ retiredEnvCtx) bool { return value == "1" }

// modeDeleted is the differs of a retired family mode: the sweeper acted on
// prune and export+prune only, so every other value was the default, off.
func modeDeleted(value string, _ retiredEnvCtx) bool { return retentionModeDeletes(value) }

// positiveOtherThan is the differs of a variable whose old read took a
// positive whole number and ignored everything else, leaving dflt.
func positiveOtherThan(dflt int64) func(string, retiredEnvCtx) bool {
	return func(value string, _ retiredEnvCtx) bool {
		n, set, ok := retiredInt(value)
		return set && ok && n > 0 && n != dflt
	}
}

// intOtherThan is the differs of a variable whose old read took any whole
// number and ignored a value that was not one.
func intOtherThan(dflt int64) func(string, retiredEnvCtx) bool {
	return func(value string, _ retiredEnvCtx) bool {
		n, set, ok := retiredInt(value)
		return set && ok && n != dflt
	}
}

// positiveFloatOtherThan is positiveOtherThan for a variable read as a
// number with a fraction.
func positiveFloatOtherThan(dflt float64) func(string, retiredEnvCtx) bool {
	return func(value string, _ retiredEnvCtx) bool {
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return err == nil && f > 0 && f != dflt
	}
}

func providerDeclared(id string) func(retiredEnvCtx) bool {
	return func(c retiredEnvCtx) bool {
		_, ok := c.cfg.Providers[id]
		return ok
	}
}

// ollamaLocalUsable reports whether the local Ollama provider is declared and
// not switched off (OLLAMA_BASE_URL=disabled), the two conditions under which
// it is registered at all.
func ollamaLocalUsable(c retiredEnvCtx) bool {
	return providerDeclared("ollama-local")(c) && c.env["OLLAMA_BASE_URL"] != "disabled"
}

// providerOptionApplied reports whether the yaml gives provider a numeric
// option the runtime applies: a positive number. Anything else (a quoted
// number, a zero) is skipped where the option is read and the default stands,
// which before this release was the variable's value.
func providerOptionApplied(provider, option string) func(retiredEnvCtx) bool {
	return func(c retiredEnvCtx) bool {
		_, ok := providerOptionInt(c.cfg.Providers[provider], option)
		return ok
	}
}

// ProviderHasKey reports whether a declared provider's credential gate is
// met: it names no key variable (a keyless endpoint, enabled by being
// declared), or the variable it names is set. A keyed provider with no key is
// not registered, so nothing can call it.
func ProviderHasKey(pc ProviderConfig, getenv func(string) string) bool {
	return pc.APIKeyEnv == "" || getenv(pc.APIKeyEnv) != ""
}

// providersLacking returns, sorted, the providers this deployment can use
// that took the global stream timeouts and do not give option in the yaml.
// A provider counts when the config declares it, it is registered (its key
// variable is set, or it names none), and it took the global pair: not
// ollama-local, which had its own, and not code-js or a mock, which make no
// HTTP call. The default providers layer declares every built-in provider,
// used or not, so "declared" alone would ask for an option on providers this
// deployment has no key for. anthropic-oauth-dev is not a declared provider
// and has no options: its timeouts are fixed.
func providersLacking(c retiredEnvCtx, option string) []string {
	var ids []string
	for id, pc := range c.cfg.Providers {
		if id == "ollama-local" || pc.Driver == "code-js" || pc.Driver == "mock" {
			continue
		}
		if !ProviderHasKey(pc, func(name string) string { return c.env[name] }) {
			continue
		}
		if _, ok := providerOptionInt(pc, option); !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

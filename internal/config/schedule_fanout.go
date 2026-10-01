package config

// ConsolidationFanoutMetadataKey is the scheduled-run metadata key that turns a
// schedule into a memory consolidation fan-out: one run per user with new work
// instead of one run. The scheduler fires on it and the ScheduleDef tool vets
// who may author it, so both read it through IsConsolidationFanout here — the
// one package both already import.
const ConsolidationFanoutMetadataKey = "memory_consolidation_fanout"

// IsConsolidationFanout reports whether a schedule's metadata carries the
// consolidation fan-out marker.
func IsConsolidationFanout(meta map[string]any) bool {
	v, ok := meta[ConsolidationFanoutMetadataKey]
	if !ok {
		return false
	}
	// YAML/JSON round-trips a bool as bool; accept the string spellings too so
	// a hand-edited substrate def does not silently disable the fan-out.
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	default:
		return false
	}
}

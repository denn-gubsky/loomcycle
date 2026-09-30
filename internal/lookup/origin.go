package lookup

// Origin records who authored a resolved definition. It exists for defs whose
// fields a runtime author controls but whose USE spends operator trust — a
// remote memory backend or document source dials its base_url carrying an
// operator credential, so whether that host may be private must not be the
// def's own say-so.
//
// It is derived from WHERE the def was read, never from a field in the def
// body: the static config, or a substrate row the def tool bootstrapped from
// the static config.
type Origin int

const (
	// OriginRuntime is a def authored over the def tools (a tenant operator,
	// or an agent granted the tool). The zero value, so an Origin nobody set
	// is the least-trusted one.
	OriginRuntime Origin = iota
	// OriginOperator is a def declared in the operator's YAML — read from the
	// static config, or from the row a fork bootstrapped from it while its
	// base_url still matches what the operator declares today.
	OriginOperator
)

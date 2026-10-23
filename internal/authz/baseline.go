package authz

// Baseline ACLs express today's principal classes as rule lists (RFC EY
// §11): what a scope-gated token may do, written in the catalog's
// vocabulary. They are code constants, visible read-only in the console,
// and what compat mode falls back to. A drift test at the HTTP layer (RFC
// EZ §2) holds that, for every principal class and surface, the baseline's
// answer equals the legacy gate's; that test is what lets the legacy tables
// be deleted.
//
// Names carry the `builtin:` prefix so a tenant-authored list can never
// shadow one: ValidName admits `:`, and the store refuses the prefix on a
// write.
const (
	BaselineAdmin          = "builtin:admin"
	BaselineTenantOperator = "builtin:tenant-operator"
	BaselineMember         = "builtin:member"
	BaselineIsolated       = "builtin:isolated"
	BaselineRunPlane       = "builtin:run-plane"
)

// BuiltinPrefix marks a baseline name.
const BuiltinPrefix = "builtin:"

func allowAll(types ...string) []Rule {
	var rules []Rule
	for _, t := range types {
		spec := catalogByName[t]
		rules = append(rules, Rule{Effect: Allow, Actions: append([]string(nil), spec.Verbs...), Resources: []string{t + ":*"}})
	}
	return rules
}

// everyType is every catalog type; the admin baseline allows every verb of
// each.
func everyType() []string { return Types() }

// Baselines returns the built-in lists by name. Each validates against the
// catalog (TestBaselines_Validate), so a catalog change that drops a verb a
// baseline names fails the build.
func Baselines() map[string]ACL {
	self := true
	return map[string]ACL{
		BaselineAdmin: {Name: BaselineAdmin, Description: "substrate:admin — every verb on every type",
			Rules: allowAll(everyType()...)},
		BaselineTenantOperator: {Name: BaselineTenantOperator, Description: "substrate:tenant — every verb in its own tenant except the operator plane",
			Rules: append(allowAll("agent", "volume", "tool", "skill", "mcp", "mcp-tool", "a2a", "client", "localapi", "search",
				"memory", "sqlmem", "document", "history", "channel", "def", "team", "credential", "crypto", "model", "tier",
				"gateway", "evaluation", "interruption", "run", "transport", "git"),
				Rule{Effect: Allow, Actions: []string{"read", "manage", "mint"}, Resources: []string{"principal:user:*", "principal:group:*"}},
				Rule{Effect: Allow, Actions: []string{"read"}, Resources: []string{"principal:token:*"}},
				Rule{Effect: Allow, Actions: []string{"read", "manage"}, Resources: []string{"runtime:limits", "runtime:retention", "runtime:erasure", "runtime:usage", "runtime:routing", "runtime:events"}},
			)},
		BaselineMember: {Name: BaselineMember, Description: "a non-isolated member — the collaboration plane, not the operator controls",
			Rules: []Rule{
				{Effect: Allow, Actions: []string{"run", "read", "spawn", "steer", "cancel", "review"}, Resources: []string{"agent:*"}},
				{Effect: Allow, Actions: []string{"read", "write", "mount"}, Resources: []string{"volume:*"}},
				{Effect: Allow, Actions: []string{"invoke"}, Resources: []string{"tool:*", "mcp:*", "mcp-tool:*", "a2a:*", "client:*", "localapi:*"}},
				{Effect: Allow, Actions: []string{"invoke", "read", "author"}, Resources: []string{"skill:*"}},
				{Effect: Allow, Actions: []string{"use"}, Resources: []string{"search:*", "model:*", "tier:*", "gateway:*", "transport:*"}},
				{Effect: Allow, Actions: []string{"read", "write", "recall"}, Resources: []string{"memory:user", "memory:agent", "memory:tenant"}},
				{Effect: Allow, Actions: []string{"browse"}, Resources: []string{"memory:user", "memory:agent"}, When: &Condition{OwnerIsSelf: &self}},
				{Effect: Allow, Actions: []string{"read", "write"}, Resources: []string{"sqlmem:user", "sqlmem:agent", "sqlmem:tenant"}},
				{Effect: Allow, Actions: []string{"read", "write", "delete", "link"}, Resources: []string{"document:user", "document:agent", "document:tenant"}},
				{Effect: Allow, Actions: []string{"browse"}, Resources: []string{"document:user", "document:agent"}, When: &Condition{OwnerIsSelf: &self}},
				{Effect: Allow, Actions: []string{"read"}, Resources: []string{"history:self", "history:user"}},
				{Effect: Allow, Actions: []string{"publish", "subscribe", "ack", "await", "broadcast"}, Resources: []string{"channel:*"}},
				{Effect: Allow, Actions: []string{"author", "retire", "read"}, Resources: []string{"def:agent/*", "def:skill/*", "def:team/*", "def:mcpserver/*", "def:schedule/*", "def:webhook/*", "def:volume/*", "def:a2a/*", "def:a2aserver/*", "def:hook/*", "def:memorybackend/*", "def:documentsource/*"}},
				{Effect: Allow, Actions: []string{"run", "read", "cancel", "author"}, Resources: []string{"team:*"}},
				{Effect: Allow, Actions: []string{"use", "manage"}, Resources: []string{"credential:user:*", "credential:agent:*"}},
				{Effect: Allow, Actions: []string{"use"}, Resources: []string{"credential:tenant:*"}},
				{Effect: Allow, Actions: []string{"submit", "read"}, Resources: []string{"evaluation:*"}},
				{Effect: Allow, Actions: []string{"raise", "resolve"}, Resources: []string{"interruption:*"}},
				{Effect: Allow, Actions: []string{"read", "steer", "cancel", "compact", "retune", "review"}, Resources: []string{"run:own", "run:tenant"}},
				{Effect: Allow, Actions: []string{"read"}, Resources: []string{"principal:user:*"}},
				{Effect: Allow, Actions: []string{"read"}, Resources: []string{"runtime:limits", "runtime:usage", "runtime:routing", "runtime:events"}},
				{Effect: Allow, Actions: []string{"fetch", "push"}, Resources: []string{"git:*"}},
			}},
		BaselineIsolated: {Name: BaselineIsolated, Description: "substrate:user — own scope only, plus the shared catalog floor",
			Rules: []Rule{
				{Effect: Allow, Actions: []string{"run", "read", "spawn", "steer", "cancel", "review"}, Resources: []string{"agent:*"}},
				{Effect: Allow, Actions: []string{"read", "write", "mount"}, Resources: []string{"volume:*"}},
				{Effect: Allow, Actions: []string{"invoke"}, Resources: []string{"tool:*", "mcp:*", "a2a:*", "client:*", "localapi:*"}},
				{Effect: Allow, Actions: []string{"invoke", "read"}, Resources: []string{"skill:*"}},
				{Effect: Allow, Actions: []string{"use"}, Resources: []string{"search:*", "model:*", "tier:*"}},
				{Effect: Allow, Actions: []string{"read", "write", "recall"}, Resources: []string{"memory:user", "memory:agent"}},
				{Effect: Allow, Actions: []string{"read", "write"}, Resources: []string{"sqlmem:user", "sqlmem:agent"}},
				{Effect: Allow, Actions: []string{"read", "write", "delete", "link"}, Resources: []string{"document:user", "document:agent"}},
				{Effect: Allow, Actions: []string{"read"}, Resources: []string{"history:self", "history:user"}},
				{Effect: Allow, Actions: []string{"use", "manage"}, Resources: []string{"credential:user:*"}, When: &Condition{OwnerIsSelf: &self}},
				{Effect: Allow, Actions: []string{"submit", "read"}, Resources: []string{"evaluation:self"}},
				{Effect: Allow, Actions: []string{"read", "steer", "cancel", "compact", "retune", "review"}, Resources: []string{"run:own"}},
				{Effect: Deny, Actions: []string{"read", "write", "recall", "browse"}, Resources: []string{"memory:tenant", "memory:global"}},
				{Effect: Deny, Actions: []string{"read", "write"}, Resources: []string{"sqlmem:tenant"}},
				{Effect: Deny, Actions: []string{"read", "write", "delete", "link", "browse"}, Resources: []string{"document:tenant"}},
				{Effect: Deny, Actions: []string{"read"}, Resources: []string{"history:tenant", "history:global"}},
				{Effect: Deny, Actions: []string{"use"}, Resources: []string{"gateway:*"}},
			}},
		BaselineRunPlane: {Name: BaselineRunPlane, Description: "a token limited to certain agents (RFC EF): the run plane only",
			Rules: []Rule{
				{Effect: Deny, Actions: []string{"author", "retire"}, Resources: []string{"def:*"}},
				{Effect: Deny, Actions: []string{"run", "author"}, Resources: []string{"team:*"}},
				{Effect: Deny, Actions: []string{"use"}, Resources: []string{"gateway:*"}},
				{Effect: Deny, Actions: []string{"manage"}, Resources: []string{"def:schedule/*", "def:webhook/*"}},
				{Effect: Deny, Actions: []string{"delete"}, Resources: []string{"run:*"}},
			}},
	}
}

// IsBuiltin reports whether name is a baseline list's.
func IsBuiltin(name string) bool {
	return len(name) > len(BuiltinPrefix) && name[:len(BuiltinPrefix)] == BuiltinPrefix
}

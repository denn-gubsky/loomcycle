package http

// overridability records, for every field of config.AgentDef, whether a RUN may
// choose its own value.
//
// This table is the security boundary of the per-run override feature, and it
// is a table rather than a convention for one reason: config.AgentDef has fifty
// fields and grows. Without somewhere that every field must appear, a new one
// arrives unclassified, and "unclassified" in a feature like this quietly means
// "nobody decided" — which is how a reach-granting field ends up caller-settable
// because it looked like tuning.
//
// TestOverridability_EveryAgentDefFieldIsClassified fails when a field is added
// and not listed here. That failure is the point: it asks the person adding it
// to decide, at the moment they have the context to decide well.
type overridability int

const (
	// notOverridable — a run may never choose this. The three groups below are
	// not interchangeable and the reasons differ; see the table.
	notOverridable overridability = iota

	// runMayChoose — a run may set this freely. These change what one run does
	// or costs, never what it may reach.
	runMayChoose

	// runMayNarrow — a run may only make this SMALLER than the definition
	// allows. Widening is refused.
	runMayNarrow
)

// agentDefOverridability classifies every config.AgentDef field.
//
// The grouping comments are load-bearing: they say WHY, and a future reader
// deciding where a new field goes needs the reason more than the verdict.
var agentDefOverridability = map[string]overridability{
	// --- routing: which model serves the run, chosen within what the
	// definition declares (RFC DC P1) ---
	"Model":    runMayChoose,
	"Provider": runMayChoose,
	"Tier":     runMayChoose,
	"Effort":   runMayChoose,

	// --- budget: what one run costs (RFC DC P2) ---
	"MaxTokens":           runMayChoose,
	"MaxIterations":       runMayChoose,
	"UnboundedIterations": runMayChoose,
	// The ONLY bound on sub-agent fan-out that exists anywhere — a child takes
	// no admission slot, is not budget-checked at spawn, and skips the
	// per-provider gate for its parent's provider.
	"MaxConcurrentChildren": runMayNarrow,

	// --- tuning: how the run is shaped, not what it may reach ---
	"Sampling": runMayChoose,
	// Which tool the model must call shapes the run; it grants no reach — the
	// named tool must already be one the run holds, and the loop refuses one
	// it does not.
	"ToolChoice": runMayChoose,
	// The shape of the answer; grants no reach.
	"OutputFormat":          runMayChoose,
	"Compaction":            runMayChoose,
	"Context":               runMayChoose,
	"MaxContextTokens":      runMayChoose,
	"RunTimeoutSeconds":     runMayChoose,
	"RetryAttempts":         runMayChoose,
	"MemoryInjectMaxTokens": runMayChoose,
	"MemoryIndexMaxBytes":   runMayChoose,
	"InjectToolGuide":       runMayChoose,
	// MOVED OUT OF "reach". It sat beside MemoryScopes and Volumes under "what
	// data and hosts the agent can touch", and it touches neither: an
	// interruption blocks and waits for a PERSON. The exposure is liveness — a
	// run that stops and waits — which is the class unbounded_iterations is in,
	// and which run_timeout_seconds and the interruption's own timeout bound.
	//
	// The operational need settled it: an operator watching an agent go wrong
	// could not let it ask a question, because that decision was frozen into the
	// definition before the run existed. An untrusted trigger still cannot set
	// it — webhook / A2A / scheduled runs build their input from the definition.
	"Interruption": runMayChoose,

	// --- narrowing-only: a run may give up reach it was granted, never take
	// more. Restoring these on resume makes a resumed run no WIDER than the
	// original, which is why RFC DD persists them. ---
	"Tools": runMayNarrow,

	// --- reach: what data and hosts the agent can touch. A caller who could
	// set these could grant themselves access the operator never gave. ---
	"Volumes":          notOverridable,
	"MemoryScopes":     notOverridable,
	"SqlScopes":        notOverridable,
	"HistoryScope":     notOverridable,
	"EvaluationScopes": notOverridable,
	"Channels":         notOverridable,
	"MemoryRoots":      notOverridable,
	"MemoryQuotaBytes": notOverridable,
	"SqlQuotaBytes":    notOverridable,
	"DisableContext":   notOverridable,
	"SearchProviders":  notOverridable,
	// A caller-chosen backend base URL is an exfiltration surface — the mem9
	// SSRF class. Never per-run.
	"MemoryBackend": notOverridable,
	// RECALL-ATTACHED TURNS ARE HISTORY REACH WEARING A MEMORY HAT. Setting it
	// causes chat transcript to be returned through the memory path, so a run that
	// could set it would obtain words the operator may have declined to grant via
	// history_scope. The tool enforces history_scope as well — both must say yes —
	// but the DECLARATION of whether turns are offered is the operator's, like every
	// other reach field here.
	"RecallIncludeTurns": notOverridable,

	// SAME REACH, WIDER: the question-anchored search returns raw turns matched to
	// the caller's own query rather than only turns a fact was extracted from, so a
	// run that could set it would reach MORE transcript than the sibling grants, not
	// less. history_scope still gates the read, but the declaration of whether the
	// turns are offered at all is the operator's — and a per-run override would
	// recreate at the API boundary exactly the failure this grant exists to avoid,
	// where the decision sits with the caller instead of the operator.
	"RecallAttachTraces": notOverridable,

	// --- authoring authority: what the agent may CREATE. Already excluded
	// from content_sha256 as "authority, not content". ---
	"AgentDefScopes":         notOverridable,
	"ScheduleDefScopes":      notOverridable,
	"A2AServerCardDefScopes": notOverridable,
	"A2AAgentDefScopes":      notOverridable,
	"SkillDefScopes":         notOverridable,
	"VolumeDefScopes":        notOverridable,
	"Hooks":                  notOverridable,
	"ToolHooks":              notOverridable,

	// --- identity and instructions: letting a caller replace these lets them
	// replace the agent, which is the whole point of a definition being a
	// definition. ---
	"SystemPrompt":        notOverridable,
	"SystemPromptBase":    notOverridable,
	"SystemPromptFile":    notOverridable,
	"Code":                notOverridable,
	"CoreBlocks":          notOverridable,
	"InheritCoreBlocks":   notOverridable,
	"Skills":              notOverridable,
	"Internal":            notOverridable,
	"MemoryProtocol":      notOverridable,
	"MemoryConsolidation": notOverridable,
	// Gates the widened {{memory:…}} / {{tool:…}} argument forms, and is
	// stamped from the runtime's own view of the caller. A per-run override of
	// it would hand an attacker the guard's key.
	"OperatorAuthored": notOverridable,
	"OwnerTenant":      notOverridable,

	// --- operator configuration (D6): the declarations themselves are the
	// operator's; an override selects WITHIN them and may not change them. ---
	"Providers": notOverridable,
	"Models":    notOverridable,
}

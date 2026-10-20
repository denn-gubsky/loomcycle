package main

import (
	"context"
	"net/http"
	"time"

	a2aapi "github.com/denn-gubsky/loomcycle/internal/api/a2a"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	webhookapi "github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/memory/reranker"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/search"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// boot carries what the server's boot phases hand to one another. main runs
// the phases in order; each reads what an earlier one left here and adds its
// own. A dependency built late but needed by something built earlier (a
// closure that runs only once the process serves) is read from here at call
// time, never copied, so it sees the value assigned after it was built.
type boot struct {
	// From the command line.
	mcpMode     bool
	cfgPaths    []string
	presetFlags []string

	cfg          *config.Config
	otelShutdown func(context.Context) error

	// buildProviders
	secrets *secretSink
	pr      *providerResolver
	sem     *concurrency.Semaphore

	// buildTools. allTools is the agent-available catalog; later phases
	// (MCP, A2A, the decision models) append to it until the server is built.
	allTools          []tools.Tool
	staticHosts       []string
	searchRegistry    *search.Registry
	searchResolver    *search.Resolver
	searchHostKeys    map[string]string
	skillSet          *skills.Set
	helpSet           *help.Set
	embedder          providers.Embedder
	bashboxTool       *builtin.Bashbox
	bashTool          *builtin.Bash
	skillTool         *builtin.SkillTool
	memoryTool        *builtin.Memory
	channelBus        *channels.Bus
	channelScheduler  *channels.Scheduler
	channelTool       *builtin.Channel
	agentDefTool      *builtin.AgentDef
	skillDefTool      *builtin.SkillDef
	teamDefTool       *builtin.TeamDef
	volumeDefTool     *builtin.VolumeDef
	credentialDefTool *builtin.CredentialDef
	cryptoTool        *builtin.Crypto
	pathTool          *builtin.Path
	documentTool      *builtin.Document
	historyTool       *builtin.History
	evaluationTool    *builtin.Evaluation
	interruptionTool  *builtin.Interruption
	contextTool       *builtin.Context

	// buildMCP
	dynamicMCPRegistry *mcp.DynamicRegistry
	mcpPool            *mcp.Pool
	mcpServerDefTool   *builtin.MCPServerDef
	skippedStaticMCP   map[string]bool
	mcpLazyResolver    *mcp.LazyResolver
	// credSubstitute resolves $cred:<name> tokens in an http MCP server's
	// headers against the tenant credential store, per request from the run
	// identity (RFC AR). The MCP pool's build callback is handed a pointer to
	// it, and the static servers are dialed before the credential engine
	// exists, so their clients read it at each request (lateCredSubstitute),
	// never at build. Assigned once the credential engine exists.
	credSubstitute func(ctx context.Context, s, targetURL string) (string, []string, error)

	// openAndMigrateStore
	storeIface store.Store

	// wireMemory, wireCredentials
	inProcBackend *inprocess.Backend
	accessFlusher *memory.AccessFlusher
	rerankModel   reranker.Reranker
	credSealer    *credential.Sealer
	credEngine    *credential.Engine
	peerCreds     *builtin.PeerCredentials
	credResolver  providers.CredentialResolver
	credKeyable   func(ctx context.Context, tenantID, agentName, userID, name string) bool

	// srv is the HTTP server every transport routes through. It is nil until
	// buildServer; closures built before then (registerSecret) read it here
	// at call time.
	srv *lchttp.Server

	// The rest of the server build.
	hookDefTool           *builtin.HookDef
	expiredTokenRetention time.Duration
	sqlMemMgr             *sqlmem.Manager // nil when SQL Memory is off
	sysPublisher          *channels.StorePublisher
	runStateBus           *runstate.Bus
	heartbeatSpecs        []channels.HeartbeatSpec
	heartbeatRunner       *channels.HeartbeatRunner // nil when no heartbeat channel is declared
	resolver              *resolve.Resolver
	a2aServer             *a2aapi.Server       // nil when A2A is off
	webhookReceiver       *webhookapi.Receiver // nil when webhooks are off
	httpServer            *http.Server

	// cleanups replace the defers main once held; see runCleanups.
	cleanups []func()
}

// addCleanup registers f to run when main returns.
func (b *boot) addCleanup(f func()) { b.cleanups = append(b.cleanups, f) }

// runCleanups runs the registered cleanups last first, as the defers they
// replace ran. Each is deferred, not called in a loop, so one that panics
// does not keep the others from running — again as with defers.
func (b *boot) runCleanups() {
	for _, f := range b.cleanups {
		defer f()
	}
}

// library_unified.go — v0.9.x Library v2: bearer-authed read-only
// enumeration that merges static cfg-defined entries with substrate
// rows into one envelope per entry. The shipping /v1/_*def/names
// endpoints (library_admin.go) enumerate ONLY substrate rows; this
// file's three endpoints enumerate every name the runtime knows
// about — yaml + dynamic + bootstrapped — and tag each with its
// source.
//
// Wire shape (per row):
//
//	{
//	  "name":              "researcher",
//	  "source":            "static-only" | "dynamic-only" | "both",
//	  "in_static":         true,
//	  "in_substrate":      true,
//	  "version_count":     3,
//	  "active_def_id":     "def_abc",
//	  "latest_version":    3,
//	  "last_updated":      "2026-05-20T12:00:00Z",
//	  "static_definition": { ... }    // omitempty
//	}
//
// in_static / in_substrate are the canonical booleans the UI consults
// for chip rendering; `source` is a derived convenience string.
//
// Read-only + bearer-authed via the same authMiddleware that wraps
// every /v1/_* endpoint.
package http

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// LibraryEntry is one row of the unified library envelope.
// StaticDefinition is omitted when the entry has no static (yaml /
// skills-root / cfg.MCPServers) source. Substrate counters are zero
// for static-only entries.
type LibraryEntry struct {
	Name             string          `json:"name"`
	Source           string          `json:"source"` // "static-only" | "dynamic-only" | "both"
	InStatic         bool            `json:"in_static"`
	InSubstrate      bool            `json:"in_substrate"`
	VersionCount     int             `json:"version_count"`
	ActiveDefID      string          `json:"active_def_id,omitempty"`
	LatestVersion    int             `json:"latest_version,omitempty"`
	LastUpdated      time.Time       `json:"last_updated,omitempty"`
	StaticDefinition json.RawMessage `json:"static_definition,omitempty"`
}

// libraryListResponse is the envelope returned by all three handlers.
type libraryListResponse struct {
	Entries []LibraryEntry `json:"entries"`
}

// handleListLibraryAgents serves GET /v1/_library/agents.
func (s *Server) handleListLibraryAgents(w http.ResponseWriter, r *http.Request) {
	// Substrate side — keyed by name for the merge below. Nil-safe:
	// when store is unset (tests), the substrate map stays empty and
	// only static cfg entries surface.
	subRows := map[string]store.AgentDefNameSummary{}
	if s.store != nil {
		rows, err := s.store.AgentDefListNames(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		for _, row := range rows {
			subRows[row.Name] = row
		}
	}

	entries := make([]LibraryEntry, 0, len(s.cfg.Agents)+len(subRows))
	seen := map[string]struct{}{}

	for name, def := range s.cfg.Agents {
		entry := LibraryEntry{
			Name:             name,
			InStatic:         true,
			StaticDefinition: marshalStaticAgentDef(def),
		}
		if sub, ok := subRows[name]; ok {
			entry.InSubstrate = true
			entry.VersionCount = sub.VersionCount
			entry.ActiveDefID = sub.ActiveDefID
			entry.LatestVersion = sub.LatestVersion
			entry.LastUpdated = sub.LastUpdated
		}
		entry.Source = deriveSource(entry.InStatic, entry.InSubstrate)
		entries = append(entries, entry)
		seen[name] = struct{}{}
	}
	for name, sub := range subRows {
		if _, ok := seen[name]; ok {
			continue
		}
		entries = append(entries, LibraryEntry{
			Name:          name,
			Source:        deriveSource(false, true),
			InSubstrate:   true,
			VersionCount:  sub.VersionCount,
			ActiveDefID:   sub.ActiveDefID,
			LatestVersion: sub.LatestVersion,
			LastUpdated:   sub.LastUpdated,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	writeJSONOK(w, libraryListResponse{Entries: entries})
}

// handleListLibrarySkills serves GET /v1/_library/skills.
func (s *Server) handleListLibrarySkills(w http.ResponseWriter, r *http.Request) {
	subRows := map[string]store.SkillDefNameSummary{}
	if s.store != nil {
		rows, err := s.store.SkillDefListNames(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		for _, row := range rows {
			subRows[row.Name] = row
		}
	}

	staticNames := s.skillSet.Names() // nil-safe — Set.Names handles nil receiver
	entries := make([]LibraryEntry, 0, len(staticNames)+len(subRows))
	seen := map[string]struct{}{}

	for _, name := range staticNames {
		sk, _ := s.skillSet.Get(name)
		entry := LibraryEntry{
			Name:             name,
			InStatic:         true,
			StaticDefinition: marshalStaticSkill(sk),
		}
		if sub, ok := subRows[name]; ok {
			entry.InSubstrate = true
			entry.VersionCount = sub.VersionCount
			entry.ActiveDefID = sub.ActiveDefID
			entry.LatestVersion = sub.LatestVersion
			entry.LastUpdated = sub.LastUpdated
		}
		entry.Source = deriveSource(entry.InStatic, entry.InSubstrate)
		entries = append(entries, entry)
		seen[name] = struct{}{}
	}
	for name, sub := range subRows {
		if _, ok := seen[name]; ok {
			continue
		}
		entries = append(entries, LibraryEntry{
			Name:          name,
			Source:        deriveSource(false, true),
			InSubstrate:   true,
			VersionCount:  sub.VersionCount,
			ActiveDefID:   sub.ActiveDefID,
			LatestVersion: sub.LatestVersion,
			LastUpdated:   sub.LastUpdated,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	writeJSONOK(w, libraryListResponse{Entries: entries})
}

// handleListLibraryMcpServers serves GET /v1/_library/mcp-servers.
//
// Static MCP servers get their cached discovered_tools attached when
// the pool inspector is wired AND the pool has a ready entry for the
// server. When the inspector returns nil (init pending or failed),
// the entry omits `discovered_tools` — the operator can re-check
// after the pool finishes init.
func (s *Server) handleListLibraryMcpServers(w http.ResponseWriter, r *http.Request) {
	subRows := map[string]store.MCPServerDefNameSummary{}
	if s.store != nil {
		rows, err := s.store.MCPServerDefListNames(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		for _, row := range rows {
			subRows[row.Name] = row
		}
	}

	entries := make([]LibraryEntry, 0, len(s.cfg.MCPServers)+len(subRows))
	seen := map[string]struct{}{}

	for name, srv := range s.cfg.MCPServers {
		var discoveredTools json.RawMessage
		if s.mcpPoolInspector != nil {
			discoveredTools = s.mcpPoolInspector(name)
		}
		entry := LibraryEntry{
			Name:             name,
			InStatic:         true,
			StaticDefinition: marshalStaticMCPServer(srv, discoveredTools),
		}
		if sub, ok := subRows[name]; ok {
			entry.InSubstrate = true
			entry.VersionCount = sub.VersionCount
			entry.ActiveDefID = sub.ActiveDefID
			entry.LatestVersion = sub.LatestVersion
			entry.LastUpdated = sub.LastUpdated
		}
		entry.Source = deriveSource(entry.InStatic, entry.InSubstrate)
		entries = append(entries, entry)
		seen[name] = struct{}{}
	}
	for name, sub := range subRows {
		if _, ok := seen[name]; ok {
			continue
		}
		entries = append(entries, LibraryEntry{
			Name:          name,
			Source:        deriveSource(false, true),
			InSubstrate:   true,
			VersionCount:  sub.VersionCount,
			ActiveDefID:   sub.ActiveDefID,
			LatestVersion: sub.LatestVersion,
			LastUpdated:   sub.LastUpdated,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	writeJSONOK(w, libraryListResponse{Entries: entries})
}

func deriveSource(inStatic, inSubstrate bool) string {
	switch {
	case inStatic && inSubstrate:
		return "both"
	case inStatic:
		return "static-only"
	default:
		return "dynamic-only"
	}
}

// staticAgentDefJSON mirrors the substrate shape of mergedDef +
// lookup.SubstrateAgentDef so the same renderer in the UI consumes
// both static and substrate-derived definitions. config.AgentDef
// carries yaml-only tags, which is why this shadow struct exists —
// the same conceptual fix PR #184 applied for the substrate-read
// path.
type staticAgentDefJSON struct {
	Provider              string                            `json:"provider,omitempty"`
	Model                 string                            `json:"model,omitempty"`
	Tier                  string                            `json:"tier,omitempty"`
	Effort                string                            `json:"effort,omitempty"`
	MaxTokens             int                               `json:"max_tokens,omitempty"`
	MaxIterations         int                               `json:"max_iterations,omitempty"`
	MaxConcurrentChildren int                               `json:"max_concurrent_children,omitempty"`
	SystemPrompt          string                            `json:"system_prompt,omitempty"`
	SystemPromptBase      string                            `json:"system_prompt_base,omitempty"`
	AllowedTools          []string                          `json:"allowed_tools,omitempty"`
	Skills                []string                          `json:"skills,omitempty"`
	Providers             []string                          `json:"providers,omitempty"`
	Models                map[string][]config.TierCandidate `json:"models,omitempty"`
	MemoryScopes          []string                          `json:"memory_scopes,omitempty"`
	MemoryQuotaBytes      int                               `json:"memory_quota_bytes,omitempty"`
}

func marshalStaticAgentDef(def config.AgentDef) json.RawMessage {
	b, err := json.Marshal(staticAgentDefJSON{
		Provider:              def.Provider,
		Model:                 def.Model,
		Tier:                  def.Tier,
		Effort:                def.Effort,
		MaxTokens:             def.MaxTokens,
		MaxIterations:         def.MaxIterations,
		MaxConcurrentChildren: def.MaxConcurrentChildren,
		SystemPrompt:          def.SystemPrompt,
		SystemPromptBase:      def.SystemPromptBase,
		AllowedTools:          def.AllowedTools,
		Skills:                def.Skills,
		Providers:             def.Providers,
		Models:                def.Models,
		MemoryScopes:          def.MemoryScopes,
		MemoryQuotaBytes:      def.MemoryQuotaBytes,
	})
	if err != nil {
		return nil
	}
	return b
}

type staticSkillJSON struct {
	Body         string   `json:"body,omitempty"`
	Description  string   `json:"description,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

// marshalStaticSkill projects the loader's Skill struct (which carries
// no json tags) onto the substrate-mirror shape so the UI renderer
// is source-agnostic.
func marshalStaticSkill(sk *skills.Skill) json.RawMessage {
	if sk == nil {
		return nil
	}
	b, err := json.Marshal(staticSkillJSON{
		Body:         sk.Body,
		Description:  sk.Description,
		AllowedTools: sk.AllowedTools,
	})
	if err != nil {
		return nil
	}
	return b
}

// marshalStaticMCPServer projects cfg.MCPServer onto a wire shape
// that mirrors the substrate's mcp_server_defs.definition JSON
// (transport / url / headers / discovered_tools) plus the stdio-only
// fields the substrate refuses (command / args / env / pool_size).
// AllowedTools is the operator's narrowing filter on tool exposure.
//
// discoveredTools is the marshaled JSON from PeekTools; nil = absent
// (init pending or failed). When non-nil it's already in the
// substrate-mirror shape, so we embed it via json.RawMessage.
func marshalStaticMCPServer(srv config.MCPServer, discoveredTools json.RawMessage) json.RawMessage {
	type staticMCPJSON struct {
		Transport       string            `json:"transport,omitempty"`
		URL             string            `json:"url,omitempty"`
		Headers         map[string]string `json:"headers,omitempty"`
		Command         string            `json:"command,omitempty"`
		Args            []string          `json:"args,omitempty"`
		Env             map[string]string `json:"env,omitempty"`
		PoolSize        int               `json:"pool_size,omitempty"`
		AllowedTools    []string          `json:"allowed_tools,omitempty"`
		DiscoveredTools json.RawMessage   `json:"discovered_tools,omitempty"`
	}
	b, err := json.Marshal(staticMCPJSON{
		Transport:       srv.Transport,
		URL:             srv.URL,
		Headers:         srv.Headers,
		Command:         srv.Command,
		Args:            srv.Args,
		Env:             srv.Env,
		PoolSize:        srv.PoolSize,
		AllowedTools:    srv.AllowedTools,
		DiscoveredTools: discoveredTools,
	})
	if err != nil {
		return nil
	}
	return b
}

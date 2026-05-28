package claudeimport

// walkMCP reads .claude/mcp.json and per-project <root>/.mcp.json
// files, mapping each top-level server to an MCPEntry. The recipe-
// library match path consults opts.Library (when non-nil and
// !opts.NoRecipeMatch); the --emit-recipes overlay path writes JSON
// files under opts.OverlayRoot.
//
// This stub is filled in by the mcp-mapper commit.
func walkMCP(claudeRoot string, opts WalkOptions, report *ImportReport) error {
	_ = claudeRoot
	_ = opts
	_ = report
	return nil
}

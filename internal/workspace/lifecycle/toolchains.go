package lifecycle

// OfferedToolchains is what a workspace o opens, on an engine at least as
// isolating as minEngine, may ask for (RFC HH §3): each toolchain a worker
// eligible for it carries, with its engines. Names only (dispatch).
func (m *Manager) OfferedToolchains(o Owner, minEngine string) map[string][]string {
	return m.disp.OfferedToolchains(o.TenantID, o.UserID, minEngine)
}
